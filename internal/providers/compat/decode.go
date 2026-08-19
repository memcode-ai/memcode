package compat

// decode.go — the wire → wire.Response direction (bodies, chunk assembly,
// and the error mapping onto the shared sentinels the runtime keys on).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/memcode-ai/memcode/internal/wire"
)

// decodeResponse converts a non-streamed completion body.
func decodeResponse(body ChatResponse, memcode bool) (wire.Response, error) {
	if len(body.Choices) == 0 {
		return wire.Response{}, errors.New("gateway response carried no choices")
	}
	c := body.Choices[0]
	resp := wire.Response{Model: body.Model, Backend: backendFor(memcode), StopReason: stopReasonFrom(c.FinishReason)}
	var text string
	if c.Message.Content != nil {
		text = *c.Message.Content
	}
	resp.Blocks = blocksFrom(c.Message.MemcodeOpaque, text, c.Message.ToolCalls, memcode)
	applyUsage(&resp, body.Usage)
	applyExt(&resp, body.Memcode, memcode)
	return resp, nil
}

// backendFor names the serving backend when the wire doesn't: a custom
// endpoint IS the backend (the ledger/cost views would otherwise fall back to
// a vendor label that never served the call). The memcode gateway reports its
// real backend via the response extension, so it stays untagged here.
func backendFor(memcode bool) string {
	if memcode {
		return ""
	}
	return "endpoint"
}

// blocksFrom rebuilds the internal block list: reasoning blocks re-extracted
// from memcode_opaque FIRST (their original response position — the order the
// next turn's history re-attaches them in), then text, then tool calls.
// Extraction is gated to the memcode backend, symmetric with the attach side.
func blocksFrom(opaque []json.RawMessage, text string, calls []ToolCall, memcode bool) []wire.Block {
	var blocks []wire.Block
	if memcode {
		for _, raw := range opaque {
			var b wire.Block
			if json.Unmarshal(raw, &b) != nil {
				continue // never kill a response over telemetry
			}
			if b.Type != "thinking" && b.Type != "redacted_thinking" {
				continue // only reasoning blocks ride the opaque channel
			}
			blocks = append(blocks, b)
		}
	}
	if text != "" {
		blocks = append(blocks, wire.TextBlock(text))
	}
	for _, tc := range calls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		blocks = append(blocks, wire.Block{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name,
			Input: json.RawMessage(args)})
	}
	return blocks
}

// stopReasonFrom maps the standard finish vocabulary back onto the internal
// stop reasons (the inverse of the gateway's FinishReasonFrom).
func stopReasonFrom(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// applyUsage converts the standard cache-INCLUSIVE prompt count back to the
// internal cache-EXCLUSIVE semantics. The standard shape reports only the
// cached-READ subset, so cache WRITES cannot be split back out — they fold
// into InputTokens (the sums, and therefore the window/compaction math, stay
// exact; only the read/write cache split is coarser than the legacy wire).
func applyUsage(resp *wire.Response, u *Usage) {
	if u == nil {
		return
	}
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	in := u.PromptTokens - cached
	if in < 0 {
		in = 0
	}
	resp.InputTokens, resp.OutputTokens, resp.CacheReadTokens = in, u.CompletionTokens, cached
}

// applyExt copies the memcode response extension onto the wire.Response
// fields the footer/compaction machinery reads. Gated to the memcode backend,
// like every extension.
func applyExt(resp *wire.Response, ext *MemcodeExt, memcode bool) {
	if ext == nil || !memcode {
		return
	}
	resp.BYOK = ext.Byok
	resp.FallbackReason = ext.FallbackReason
	resp.SearchCount = ext.SearchCount
	resp.ContextWindow = ext.ContextWindow
	resp.InputBudget = ext.InputBudget
	resp.Pool = ext.Pool
}

// ── stream assembly ─────────────────────────────────────────────────────────

// callAccum accumulates one streamed tool call (fragments concatenate — the
// gateway emits one full delta per call, third-party endpoints split
// arguments; both assemble identically).
type callAccum struct {
	id         string
	name, args strings.Builder
}

// streamAccum assembles chat chunks into the final wire.Response while
// forwarding live deltas to the StreamHandler.
type streamAccum struct {
	memcode bool
	text    strings.Builder
	calls   map[int]*callAccum
	order   []int
	opaque  []json.RawMessage
	finish  string
	model   string
	usage   *Usage
	ext     *MemcodeExt
}

func newStreamAccum(memcode bool) *streamAccum {
	return &streamAccum{memcode: memcode, calls: map[int]*callAccum{}}
}

// apply folds one chunk in, firing the handler's callbacks as deltas arrive.
func (a *streamAccum) apply(c ChatChunk, h wire.StreamHandler) {
	if c.Model != "" {
		a.model = c.Model
	}
	if c.Usage != nil {
		a.usage = c.Usage
		if h.Usage != nil {
			var tmp wire.Response
			applyUsage(&tmp, c.Usage) // deliver internal-semantics counts
			h.Usage(tmp.InputTokens, tmp.OutputTokens)
		}
	}
	if c.Memcode != nil {
		a.ext = c.Memcode
	}
	for _, ch := range c.Choices {
		d := ch.Delta
		if d.Content != nil && *d.Content != "" {
			a.text.WriteString(*d.Content)
			if h.Text != nil {
				h.Text(*d.Content)
			}
		}
		for _, td := range d.ToolCalls {
			ca := a.calls[td.Index]
			if ca == nil {
				ca = &callAccum{}
				a.calls[td.Index] = ca
				a.order = append(a.order, td.Index)
			}
			if td.ID != "" {
				ca.id = td.ID
			}
			if td.Function != nil {
				ca.name.WriteString(td.Function.Name)
				ca.args.WriteString(td.Function.Arguments)
			}
		}
		if len(d.MemcodeOpaque) > 0 {
			a.opaque = append(a.opaque, d.MemcodeOpaque...)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finish = *ch.FinishReason
		}
	}
}

// partialUsage returns a usage-only response for a stream that FAILED
// mid-flight: whatever the backend already reported billed before the cut
// (mirrors the native adapters' error-path partials, so the Runner can meter
// an expensive failure instead of losing it).
func (a *streamAccum) partialUsage() wire.Response {
	resp := wire.Response{Model: a.model, Backend: backendFor(a.memcode)}
	applyUsage(&resp, a.usage)
	return resp
}

// response returns the assembled result once [DONE] arrives.
func (a *streamAccum) response() wire.Response {
	sort.Ints(a.order)
	calls := make([]ToolCall, 0, len(a.order))
	for _, i := range a.order {
		ca := a.calls[i]
		calls = append(calls, ToolCall{ID: ca.id, Type: "function",
			Function: FunctionCall{Name: ca.name.String(), Arguments: ca.args.String()}})
	}
	resp := wire.Response{Model: a.model, Backend: backendFor(a.memcode), StopReason: stopReasonFrom(a.finish)}
	resp.Blocks = blocksFrom(a.opaque, a.text.String(), calls, a.memcode)
	applyUsage(&resp, a.usage)
	applyExt(&resp, a.ext, a.memcode)
	return resp
}

// ── error mapping ───────────────────────────────────────────────────────────

// mapHTTPError turns a non-200 into the shared sentinel the runtime keys on
// (compact-and-retry, fail-the-turn, signed-out) — the same status+code
// contract as the legacy wire (see the gateway's compatTurnError). Everything
// else surfaces as a "memcode api http N" error, message-first when the
// standard envelope decodes.
func mapHTTPError(status int, raw []byte) error {
	if status == http.StatusUnauthorized {
		return fmt.Errorf("%w", wire.ErrUnauthorized)
	}
	var env ErrorResponse
	decoded := json.Unmarshal(raw, &env) == nil && env.Error.Message != ""
	code := env.Error.Code
	switch {
	case status == http.StatusPaymentRequired && code == wire.CodeInsufficientCredit:
		return wire.ErrInsufficientCredit
	case status == http.StatusPaymentRequired && code == wire.CodeSubscriptionRequired:
		return wire.ErrSubscriptionRequired
	case status == http.StatusPaymentRequired && code == wire.CodeAccountLocked:
		return wire.ErrAccountLocked
	case status == http.StatusUnprocessableEntity && code == wire.CodeByokKeyFailed:
		return wire.ErrByokKeyFailed
	case status == http.StatusRequestEntityTooLarge && code == wire.CodeContextOverflow:
		return wire.ErrContextOverflow
	}
	if decoded {
		return fmt.Errorf("memcode api http %d: %s", status, env.Error.Message)
	}
	return fmt.Errorf("memcode api http %d: %s", status, strings.TrimSpace(string(raw)))
}

// streamErr maps a mid-stream error event (the standard envelope on a data:
// line) onto the same sentinels.
func streamErr(body ErrorBody) error {
	switch body.Code {
	case wire.CodeContextOverflow:
		return wire.ErrContextOverflow
	case wire.CodeInsufficientCredit:
		return wire.ErrInsufficientCredit
	case wire.CodeSubscriptionRequired:
		return wire.ErrSubscriptionRequired
	case wire.CodeAccountLocked:
		return wire.ErrAccountLocked
	case wire.CodeByokKeyFailed:
		return wire.ErrByokKeyFailed
	}
	return fmt.Errorf("memcode api: %s", body.Message)
}
