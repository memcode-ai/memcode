package runtime

// In-session context compaction (COMPACTION.md, the warm layer). memcode sends the
// full append-only history every turn; prompt caching makes that cheap but the
// window still fills, so a long session drifts off the cheap lane onto
// Anthropic at `context_over_lanes`. Compaction summarizes the OLDER turns into a
// dense block and keeps only the recent ones raw, so the prompt shrinks back under
// the lane budget — substrate-independent (it also saves a pure-Anthropic session
// from dying at the window). The cut mechanics + invariants live in the pure
// `compaction` package; this file owns the trigger, the (Anthropic) summarizer
// call, and persistence.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/compaction"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/sessionlog"
	"github.com/memcode-ai/memcode/internal/wire"
)

const (
	// windowFallbackPct sizes the PRE-LEARNING budget as a fraction of the
	// serving model's RAW catalog window (the learned lane capacity already
	// excludes the output reserve, so it gets the higher compactBudgetPct).
	// There are deliberately NO absolute token constants here — every prior
	// magic number (45K from the dead 64K lane, then 150K) aged into a silent
	// clip on models whose windows had grown 5-20x. Budgets are relative to
	// the window, the way Claude Code/Codex compact near the model's actual
	// window, or they rot.
	windowFallbackPct = 80
	// compactKeepRecent is how many of the most recent user turns stay RAW. The
	// summary covers everything older. ~the last 8 exchanges (COMPACTION.md).
	compactKeepRecent = 8
	// compactTimeout bounds the summarizer call — a long history can be a big
	// prompt, but compaction must never hang a turn; on timeout we degrade to the
	// full history (today's behaviour).
	compactTimeout = 90 * time.Second
	// compactMaxTokens caps the summary length — dense, not another transcript.
	compactMaxTokens = 2048
)

// keepToolResults is how many of the most recent tool_result outputs stay
// verbatim when eviction runs, SCALED to the active budget: roughly one kept
// output per 10K tokens of budget, floored at the old constant (8) and capped
// at 32. The fixed keep counts (6 in-turn / 8 cross-turn) were the thrash
// mechanism: a task's working set (9-10 files) exceeded them, so every fresh
// read evicted a file still in use and the model re-read it — 13-14x per file
// in the measured audit session, each eviction also busting the prefix cache.
func keepToolResults(budget int) int {
	k := budget / 10_000
	if k < 8 {
		return 8
	}
	if k > 32 {
		return 32
	}
	return k
}

// earlyEvictTrigger is the transcript size that arms cross-turn eviction at a
// new user turn (the Anthropic context-editing posture: clear stale tool
// results long before the emergency threshold). Scaled to the active budget —
// just under half of it, preserving the historical 20K/45K ratio.
func (s *Session) earlyEvictTrigger() int { return s.compactBudget() / 2 }

// hotPathsCap bounds the pinned working set — pins protect a re-read working
// set from thrash, but an unbounded pin set would quietly defeat the budget.
const hotPathsCap = 12

// noteHotPaths folds one turn's per-target gather counts into the session's
// HOT set: any read_file path read ≥2 times in a turn is hot (a re-read is the
// direct signal the evictor discarded something still in use). Existing scores
// decay by half each turn so stale pins expire; the set is capped at
// hotPathsCap (coldest evicted first). Called from runLoop's telemetry defer.
func (s *Session) noteHotPaths(byTarget map[string]int) {
	if s.hotPaths == nil {
		s.hotPaths = map[string]int{}
	}
	for p, v := range s.hotPaths { // decay: a path stops being hot ~2 turns after re-reads stop
		if v /= 2; v <= 0 {
			delete(s.hotPaths, p)
		} else {
			s.hotPaths[p] = v
		}
	}
	for target, n := range byTarget {
		p, ok := strings.CutPrefix(target, "read:")
		if !ok || p == "" || n < 2 {
			continue
		}
		s.hotPaths[p] += n
	}
	for len(s.hotPaths) > hotPathsCap { // cap: drop the coldest
		coldest, min := "", 0
		for p, v := range s.hotPaths {
			if coldest == "" || v < min {
				coldest, min = p, v
			}
		}
		delete(s.hotPaths, coldest)
	}
}

// pinnedPredicate returns the evictor's hot-path test, or nil when nothing is
// hot (the common case — zero overhead).
func (s *Session) pinnedPredicate() func(string) bool {
	if len(s.hotPaths) == 0 {
		return nil
	}
	hp := s.hotPaths
	return func(path string) bool { return hp[path] >= 2 }
}

// evictOnTurnStart clears stale tool-result payloads at the start of a new user
// turn once the transcript is past earlyEvictTrigger. Deterministic, no model
// call, adjacency-safe (payload→pointer swap only). No-op in plan mode (research
// stays raw for synthesis; see offloadResearchForSynthesis).
func (s *Session) evictOnTurnStart(messages *[]wire.Message) {
	if s.planCtl.Planning() || compaction.EstimateTokens(*messages) <= s.earlyEvictTrigger() {
		return
	}
	if n := compaction.EvictStaleToolResultsOpts(*messages, keepToolResults(s.compactBudget()),
		compaction.EvictOpts{Pinned: s.pinnedPredicate()}); n > 0 {
		s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⊙ cleared %d stale tool output(s) from earlier turns (re-readable on demand)", n)))
	}
}

// manageInTurnContext bounds a long agentic turn's context at a safe boundary (after
// tool_results, before the next model call). First it elides stale tool_result content
// (cheap, no model call, adjacency-safe); if still over budget, it compacts the older
// turns. This is what stops a 40-tool-call turn from ballooning to millions of tokens and
// degrading into an empty response. No-op in plan mode (research is the raw material for
// synthesis) or when compaction is disabled.
func (s *Session) manageInTurnContext(ctx context.Context, messages *[]wire.Message) {
	budget := s.compactBudget()
	if s.planCtl.Planning() || budget <= 0 || compaction.EstimateTokens(*messages) <= budget {
		return
	}
	if n := compaction.EvictStaleToolResultsOpts(*messages, keepToolResults(budget),
		compaction.EvictOpts{Pinned: s.pinnedPredicate()}); n > 0 {
		s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⊙ trimmed %d stale tool output(s) to keep the working set lean", n)))
	}
	if est := compaction.EstimateTokens(*messages); est > budget && s.compactWouldHelp(est) {
		// Proactive keep-count (8 recent turns), NOT the emergency keep=2: the
		// aggressive count on this routine path rewrote nearly the whole history
		// on every budget crossing — a full prefix-cache bust and the "repeated
		// near-identical compactions" signature. keep=2 stays reserved for the
		// reactive ErrContextOverflow retry in runLoop.
		s.compactMessages(ctx, messages, compactKeepRecent, false)
	}
}

// compactWouldHelp is the compaction BACK-OFF: when the last pass could not get
// under budget (an irreducibly large recent working set), re-summarizing the
// same history every boundary just burns a summarizer call and rewrites the
// prefix (cache bust) for a near-identical result — the measured "3 compactions
// in 8 minutes" signature. Re-compact only after ≥20% real growth past the last
// pass's result. Manual /compact resets the baseline (the user asked).
func (s *Session) compactWouldHelp(est int) bool {
	return s.lastCompactAfter == 0 || est >= s.lastCompactAfter*120/100
}

// offloadResearchForSynthesis elides stale research tool-output ONCE, right before plan synthesis —
// the single place plan mode prunes (the research loop itself stays raw; see manageInTurnContext's
// plan-mode no-op). Older tool_results become typed, re-fetchable pointers (lossless), so the
// synthesis call — and every draft/review iteration that re-sends *messages after it — stops dragging
// the entire raw research transcript. The findings the plan is built from live in the prose, which is
// untouched. Keep-count scales with the budget like every other eviction site.
func (s *Session) offloadResearchForSynthesis(messages *[]wire.Message) {
	if n := compaction.EvictStaleToolResultsOpts(*messages, keepToolResults(s.compactBudget()),
		compaction.EvictOpts{Pinned: s.pinnedPredicate()}); n > 0 {
		s.printf("%s\n", metaStyle.Render(fmt.Sprintf("  ⊙ offloaded %d stale research output(s) before synthesis (re-readable on demand)", n)))
	}
}

// compactBudgetPct is the fraction of the cheap lane's input budget that compaction
// aims under, leaving headroom so a compacted prompt sits comfortably inside the lane
// (not right at its edge, where the next turn's growth would spill it again).
const compactBudgetPct = 85

// compactBudget is the active token budget; 0 disables compaction. Precedence:
// MEMCODE_COMPACT_BUDGET (explicit override / "off") → the learned lane
// capacity × 85% — the WHOLE window, minus headroom, is the budget: context
// pressure (evictions, compactions, the cache busts and re-reads they cause)
// is the expensive failure, and resident tokens ride cheaply as cache reads
// ($0.26/M on the cheap lane ≈ $0.26/iteration even at a full 1M). An optional
// MEMCODE_CONTEXT_SOFT_CAP lowers the ceiling for cost-capped setups; there is
// deliberately no built-in clip. Static default before any lane turn has
// revealed the capacity.
func (s *Session) compactBudget() int {
	switch v := strings.TrimSpace(os.Getenv("MEMCODE_COMPACT_BUDGET")); v {
	case "":
		if b := s.servedSnapshot().inputBudget; b > 0 {
			if cap := envInt("MEMCODE_CONTEXT_SOFT_CAP"); cap > 0 && cap < b {
				b = cap
			}
			return b * compactBudgetPct / 100
		}
		return s.windowFallbackBudget()
	case "off", "0":
		return 0
	default:
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		return s.windowFallbackBudget()
	}
}

// windowFallbackBudget is the pre-learning budget: a fraction of the serving
// model's real catalog window (ContextWindow prefers the gateway-reported
// serving window, else the model's catalog size — never an absolute number).
func (s *Session) windowFallbackBudget() int {
	b := s.ContextWindow() * windowFallbackPct / 100
	if cap := envInt("MEMCODE_CONTEXT_SOFT_CAP"); cap > 0 && cap < b {
		b = cap
	}
	return b
}

// envInt reads a positive integer env var (0 when unset/invalid).
func envInt(name string) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// compactIfNeeded compacts the running history when its estimated size exceeds the
// budget, at a safe turn boundary BEFORE the next turn is assembled (so the smaller
// prompt is what gets routed — keeping the turn on the cheap lane). Plan mode is
// exempt: its research history is the raw material for synthesis. Best-effort — any
// failure leaves the history untouched.
func (s *Session) compactIfNeeded(ctx context.Context, st *ChatState) {
	if s.planCtl.Planning() || s.compactBudget() <= 0 {
		return
	}
	est := compaction.EstimateTokens(st.messages)
	if est <= s.compactBudget() || !s.compactWouldHelp(est) {
		return
	}
	if line, ok := s.compact(ctx, st, false); ok {
		s.printf("%s\n", metaStyle.Render("  "+line))
	}
}

// Compact is the manual /compact entry point: force a compaction now and return a
// one-line result for the front-end to print (success line, or why it was a no-op).
func (s *Session) Compact(ctx context.Context, st *ChatState) string {
	s.lastCompactAfter = 0 // the user asked — reset the back-off baseline
	line, _ := s.compact(ctx, st, true)
	return line
}

// compact summarizes the older turns of st.messages, keeping the recent turns raw.
func (s *Session) compact(ctx context.Context, st *ChatState, manual bool) (string, bool) {
	return s.compactMessages(ctx, &st.messages, compactKeepRecent, manual)
}

// compactKeepRecentOnOverflow is the AGGRESSIVE keep-count used by the reactive
// overflow path (loop.go): a turn that overflowed even Anthropic's window must shrink
// hard, so keep far fewer recent turns than the proactive budget trigger does.
const compactKeepRecentOnOverflow = 2

// compactMessages summarizes the older turns of a message slice into a single warm
// block and keeps the last keepRecent turns raw. The summarizer is ALWAYS Anthropic
// (force-escalate): a wrong summary silently becomes the session's false memory, so
// v1 never trusts the cheap pool with it (COMPACTION.md hard rule #1). It operates on
// *[]wire.Message so both the ChatState path (proactive /compact + auto) and the
// runLoop overflow path (reactive) share one implementation. Returns a status line
// and whether the history actually changed.
func (s *Session) compactMessages(ctx context.Context, messages *[]wire.Message, keepRecent int, manual bool) (string, bool) {
	head, tail, ok := compaction.Plan(*messages, keepRecent)
	if !ok {
		return "nothing to compact yet — the conversation is still short.", false
	}
	before := compaction.EstimateTokens(*messages)
	transcript := s.redactor.Redact(compaction.Render(head))

	cctx, cancel := context.WithTimeout(ctx, compactTimeout)
	defer cancel()
	// Mode "compact" selects the compactor doctrine (composed client-side by the
	// transport); the rendered transcript rides as the user message. The ladder's
	// purpose switch routes compact to the strong balanced tier on its own
	// (llm/lane.go) — no hint needed, and none is read for utility purposes.
	resp, err := s.sideComplete(cctx, llm.Compact, wire.Request{
		Mode:      "compact",
		MaxTokens: compactMaxTokens,
		Messages:  []wire.Message{{Role: "user", Blocks: []wire.Block{{Type: "text", Text: transcript}}}},
	})
	if err != nil {
		if ctx.Err() != nil { // the user cancelled the turn — not a failure
			return "compaction cancelled.", false
		}
		return "compaction failed (keeping full history): " + err.Error(), false
	}
	summary := strings.TrimSpace(resp.Text())
	if summary == "" {
		return "compaction produced no summary — leaving history as-is.", false
	}

	*messages = newCompactedHistory(summary, tail)
	after := compaction.EstimateTokens(*messages)
	turns := compaction.CountTurns(head)

	s.lastCompactSummary = summary
	s.lastCompactAfter = after // back-off baseline: don't re-compact until real regrowth past this
	s.logCompaction(summary)
	s.emit(ctx, events.KindContextCompacted, map[string]any{
		"raw_history_tokens":       before,
		"summary_tokens":           estimateTokens(len(summary)),
		"compacted_turns":          turns,
		"context_after_compaction": after,
		"ineffective":              after > s.compactBudget(), // couldn't get under budget — back-off suppresses retries
		"manual":                   manual,
	})
	return fmt.Sprintf("⊙ compacted context — summarized %d earlier turn(s) (≈%s → ≈%s tokens)",
		turns, kTokens(before), kTokens(after)), true
}

// compactedMarker labels the synthetic summary turn so it's unmistakable in the
// history (and so a SECOND compaction folds it into the next summary). It also
// signposts the COLD layer: the raw older turns left the window but are still on
// disk, retrievable on demand — like reading a code file you don't have in context.
const compactedMarker = "[Earlier conversation compacted to the summary below to save context. The session log " +
	"durably records the earlier ASKS, decisions, commands run, and edits — retrieve those with " +
	"memcode{command:\"session\", target:\"search\", query:\"…\"} (or target:\"current\") rather than saying you " +
	"don't remember. IMPORTANT: raw tool OUTPUT (a grep's hits, a file's contents, a test's results) is NOT " +
	"stored — only the commands themselves. If you need a specific earlier result that isn't in the summary, " +
	"RE-RUN the read/grep/test to get it fresh; do NOT reconstruct it from memory and present it as verified.]"

// newCompactedHistory builds the replacement message list: a synthetic user turn
// carrying the summary, an assistant acknowledgement (so alternation holds and the
// kept tail — which starts with a real user turn — follows an assistant turn),
// then the verbatim tail (preserving its tool pairs and thinking signatures).
func newCompactedHistory(summary string, tail []wire.Message) []wire.Message {
	out := make([]wire.Message, 0, len(tail)+2)
	out = append(out,
		wire.Message{Role: "user", Blocks: []wire.Block{{Type: "text", Text: compactedMarker + "\n\n" + summary}}},
		wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "Understood — I'll continue from that summary."}}},
	)
	return append(out, tail...)
}

// logCompaction records the warm summary in the episodic log so /recap, recall, and
// a later session can see it (COMPACTION.md hard rule #3).
func (s *Session) logCompaction(summary string) {
	if s.slog == nil {
		return
	}
	s.slog.Append(sessionlog.Record{Kind: sessionlog.KindCompaction, Text: s.redactor.Redact(summary)})
}

// kTokens renders an estimated token count compactly ("1.2k", "850").
func kTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
