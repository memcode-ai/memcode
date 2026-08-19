package runtime

// judge.go — the ONE shared plumbing layer for side-channel LLM calls (classifiers,
// judges, background utilities): every call that is NOT the main working-model turn goes
// through sideComplete, so it is wire-traced (MEMCODE_TRACE) and — for classify-purpose
// judges — failure-counted with timeout/error distinction. Before this existed, all 11
// side-channel pipelines hand-rolled the same Complete+scan+Unmarshal plumbing and failed
// SILENTLY: a turn_intent judge timing out on every message was indistinguishable from
// one that never fired, which is exactly the blindness that made the 5s→30s timeout bump
// unverifiable. The judgment DOCTRINE stays server-side per mode (prompts.go); this file
// owns only transport, decoding, and observability.
//
// Architecture note (the "fold classifiers into the working model?" question): routing
// judges (turn_intent, followup_intent, plan_followup_intent) decide whether and how the
// working model runs at all, so they cannot live inside it; authorize is deliberately
// adversarial to the agent (the judge must not see its reasoning); plan_review is
// cross-model by design. Side-channel is the architecture — this file removes the sprawl.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/doctor"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/wire"
)

// errNoVerdict — the model answered but no usable verdict could be decoded (wrong/missing
// tool_use block AND no parseable prose JSON). Counted as a failure, distinct from timeout.
var errNoVerdict = errors.New("classifier returned no usable verdict")

// errVerdictTruncated — the response hit the max_tokens cap before the tool call finished
// (the reasoning-lane trap classifyMaxTokens exists to prevent). Distinct from errNoVerdict
// so /doctor shows WHERE the verdict went instead of a generic decode miss.
var errVerdictTruncated = errors.New("classifier verdict truncated at max_tokens")

// judgeFailNotice — after this many CONSECUTIVE classifier failures (across modes), print
// one dim notice per streak so silent degradation becomes visible without nagging.
const judgeFailNotice = 3

// judgeModeStats is one mode's traffic: successes, timeouts (context deadline), and other
// errors, plus the most recent failure for the /doctor row.
type judgeModeStats struct {
	OK, Timeout, Err int
	LastErr          string
	LastAt           time.Time
}

// judgeStats aggregates side-channel classifier outcomes per session. Own mutex, not
// Session.mu: judges run on their own goroutines (turn judge, followup classifier) and
// must never contend with output/metric locking.
type judgeStats struct {
	mu               sync.Mutex
	byMode           map[string]*judgeModeStats
	consecutiveFails int
	noticed          bool // one notice per failure streak
}

// record books one outcome and reports whether the failure-streak notice should fire now.
func (js *judgeStats) record(mode string, err error, timedOut bool) (notice bool, streakErr string) {
	js.mu.Lock()
	defer js.mu.Unlock()
	if js.byMode == nil {
		js.byMode = map[string]*judgeModeStats{}
	}
	st := js.byMode[mode]
	if st == nil {
		st = &judgeModeStats{}
		js.byMode[mode] = st
	}
	if err == nil {
		st.OK++
		js.consecutiveFails = 0
		js.noticed = false
		return false, ""
	}
	if timedOut {
		st.Timeout++
	} else {
		st.Err++
	}
	st.LastErr = err.Error()
	st.LastAt = time.Now()
	js.consecutiveFails++
	if js.consecutiveFails >= judgeFailNotice && !js.noticed {
		js.noticed = true
		return true, st.LastErr
	}
	return false, ""
}

// sideComplete is the ONE call site for side-channel LLM requests: Complete + wire trace.
// Counter bookkeeping happens in classifyToolCall (it also sees decode misses); pipelines
// that still parse prose (until they migrate) get tracing here for free.
func (s *Session) sideComplete(ctx context.Context, purpose llm.Purpose, req wire.Request) (wire.Response, error) {
	resp, err := s.runner.Complete(ctx, purpose, req)
	s.traceWire(purpose, req, resp, err)
	return resp, err
}

// recordJudge books a classifier outcome and surfaces the once-per-streak notice.
func (s *Session) recordJudge(mode string, err error, timedOut bool) {
	notice, lastErr := s.judges.record(mode, err, timedOut)
	if notice {
		s.printf("%s\n", metaStyle.Render(fmt.Sprintf(
			"  ⊙ background classifiers failing (%d×, last: %s) — routing falls back to safe defaults; details in /doctor",
			judgeFailNotice, clip(lastErr, 120))))
	}
}

// classifyToolCall runs one forced-tool classify call end-to-end: timeout, redaction is
// the CALLER's job (prompt arrives ready), sideComplete on the classify lane, decode into
// out, and counter bookkeeping with timeout/error/no-verdict distinction. Fail-open
// semantics stay at the call site — this returns the error and the caller decides what a
// failure means for its pipeline.
//
// MaxTokens is deliberately 0 — UNCAPPED. The gateway translates 0 per lane: the field is
// omitted for OpenAI-compatible/Gemini backends (the model's own max applies) and resolved
// to the catalog's max_output for Anthropic (which requires the field). Judges must never
// carry a fixed output cap: the cheap classify lane can serve a REASONING model
// (gpt-oss-120b) that spends output tokens thinking before the forced tool call, and the
// old per-call caps (64–512) silently truncated the verdict JSON mid-object on every
// affected call. The wall clock is bounded by timeout, and tokens by the tiny prompt —
// a cap adds no safety, only a truncation failure mode.
func (s *Session) classifyToolCall(ctx context.Context, mode string, tool wire.ToolDef, prompt string, timeout time.Duration, out any) error {
	nctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := s.sideComplete(nctx, llm.Classify, wire.Request{
		Mode:       mode,
		Messages:   []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: prompt}}}},
		Tools:      []wire.ToolDef{tool},
		ToolChoice: tool.Name,
	})
	if err != nil {
		s.recordJudge(mode, err, errors.Is(err, context.DeadlineExceeded) || nctx.Err() == context.DeadlineExceeded)
		return err
	}
	if !decodeForcedTool(resp, tool, out) {
		err := errNoVerdict
		if resp.StopReason == "max_tokens" {
			err = errVerdictTruncated
		}
		s.recordJudge(mode, err, false)
		return err
	}
	s.recordJudge(mode, nil, false)
	return nil
}

// recentHistorySlice renders the tail of this session's episodic log — user and
// assistant messages only, oldest first — as classify context. The judges run OFF the
// engine goroutine and cannot read the live turn's message history, but the sessionlog
// writer flushes whole lines under its own lock, so a tail read of the append-only file
// is race-free and current. A judge deciding "does this message steer the current work?"
// (or synthesizing a title for it) is blind without this: the scheduler's anchor text
// can be a synthetic one-liner that says nothing about what's actually going on.
// Empty when the log doesn't exist yet (bare fixtures, brand-new session).
func (s *Session) recentHistorySlice(maxMsgs, perMsgClip, totalClip int) string {
	recs, err := sessionlog.Recent(s.root, s.sessionID, 120)
	if err != nil || len(recs) == 0 {
		return ""
	}
	var lines []string
	for _, r := range recs {
		var who string
		switch r.Kind {
		case sessionlog.KindUserMessage:
			who = "user"
		case sessionlog.KindAssistantMessage:
			who = "assistant"
		default:
			continue
		}
		if text := strings.TrimSpace(r.Text); text != "" {
			lines = append(lines, who+": "+clip(text, perMsgClip))
		}
	}
	if len(lines) > maxMsgs {
		lines = lines[len(lines)-maxMsgs:]
	}
	return clip(strings.Join(lines, "\n"), totalClip)
}

// decodeForcedTool extracts the forced tool_use verdict into out; falls back to
// best-effort {...} prose-JSON extraction (the parsePlanVerdict pattern) so a doctrine/CLI
// version skew — a gateway that didn't force the tool — never zeroes a verdict.
func decodeForcedTool(resp wire.Response, tool wire.ToolDef, out any) bool {
	for _, blk := range resp.Blocks {
		if blk.Type == "tool_use" && blk.Name == tool.Name && len(blk.Input) > 0 {
			if json.Unmarshal(blk.Input, out) == nil {
				return true
			}
		}
	}
	txt := resp.Text()
	if i, j := strings.Index(txt, "{"), strings.LastIndex(txt, "}"); i >= 0 && j > i {
		if json.Unmarshal([]byte(txt[i:j+1]), out) == nil {
			return true
		}
	}
	return false
}

// classifierChecks renders the session's side-channel classifier traffic as /doctor rows —
// one per mode that actually ran, nothing when none did. This is THE instrument for "is
// turn_intent still timing out": ok/timeout/err with the last failure's age.
func (s *Session) classifierChecks() []doctor.Result {
	s.judges.mu.Lock()
	defer s.judges.mu.Unlock()
	if len(s.judges.byMode) == 0 {
		return nil
	}
	var out []doctor.Result
	for mode, st := range s.judges.byMode {
		status := doctor.OK
		detail := fmt.Sprintf("%d ok", st.OK)
		if st.Timeout+st.Err > 0 {
			status = doctor.Warn
			detail = fmt.Sprintf("%d ok, %d timeout, %d err — last failure %s ago (%s)",
				st.OK, st.Timeout, st.Err, time.Since(st.LastAt).Round(time.Second), clip(st.LastErr, 80))
		}
		out = append(out, doctor.Result{Name: "classifier " + mode, Status: status, Detail: detail})
	}
	return out
}
