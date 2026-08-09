package compat

// lane.go — the SERVER-SIDE lane features, ported verbatim from the gateway's
// Fireworks client (the retired internal oaClient): the lane error contract,
// the model-conditional reasoning_effort vocabulary, and the tool-call salvage
// net + MiniMax leak strip for small open models. Enabled via Config.Lane /
// Config.Salvage — the same engine serves the CLI transport and the gateway's
// cheap lane, with these as configuration, not forks.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/memcode-ai/memcode/internal/providers/provcore"
	"github.com/memcode-ai/memcode/internal/wire"
)

// LaneRequestError is a non-retryable 4xx from an OpenAI-compatible lane: the
// request itself is the problem (malformed, or — most commonly — longer than
// the served context window), NOT the server. Distinguish from 5xx/timeout,
// which mean unhealthy.
type LaneRequestError struct {
	Status   int
	Message  string // full server message, never clipped
	Overflow bool   // true when the rejection is a context-length overflow
}

func (e *LaneRequestError) Error() string {
	return fmt.Sprintf("lane http %d: %s", e.Status, e.Message)
}

// mapError picks the error contract per mode: the lane contract
// (LaneRequestError for request-shaped 4xx, generic otherwise) server-side;
// the client sentinel mapping everywhere else.
func (t *Transport) mapError(status int, raw []byte) error {
	if !t.lane {
		return mapHTTPError(status, raw)
	}
	msg := strings.TrimSpace(string(raw))
	var env ErrorResponse
	if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
		msg = env.Error.Message
	}
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return &LaneRequestError{Status: status, Message: msg, Overflow: provcore.IsOverflowMessage(msg)}
	}
	return fmt.Errorf("lane http %d: %s", status, msg)
}

// laneReasoningEffort maps the abstract Effort onto a lane model's
// reasoning_effort. Policy (ported unchanged): ordinary tool-carrying turns
// reason at the model's HIGH tier, genuinely-hard turns (EffortHigh) at MAX;
// tool-less Fireworks turns send an explicit "none" (Kimi/GLM default to
// emitting a chain of thought when the field is omitted) — except gpt-oss,
// which rejects "none". "" = omit (a non-reasoning model stays untouched).
func laneReasoningEffort(model string, e wire.Effort, hasTools bool) string {
	if !supportsReasoningEffort(model) {
		return ""
	}
	if !hasTools {
		if strings.HasPrefix(model, "accounts/") && !strings.Contains(strings.ToLower(model), "gpt-oss") && e == wire.EffortOff {
			return "none"
		}
		return ""
	}
	if e == wire.EffortHigh {
		if strings.Contains(strings.ToLower(model), "grok") {
			return "high" // Grok doesn't support "max" — cap at "high"
		}
		return "max"
	}
	return "high"
}

// supportsReasoningEffort reports whether a served model honors the lane's
// reasoning_effort knob. Conservative: an unrecognized model returns false so
// we omit the field and never risk a 400 on a non-reasoning endpoint.
func supportsReasoningEffort(model string) bool {
	m := strings.ToLower(model)
	for _, s := range []string{"glm", "qwen", "deepseek", "kimi", "minimax", "gpt-oss", "grok"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// finishResponse applies the post-decode lane passes: the MiniMax leak strip,
// the tool-call salvage net, and the leak capture. A no-op without
// Config.Salvage.
func (t *Transport) finishResponse(resp wire.Response, r wire.Request, requestBody []byte) wire.Response {
	if !t.salvage {
		return resp
	}
	if isMinimaxModel(resp.Model) || isMinimaxModel(r.Model) {
		for i, b := range resp.Blocks {
			if b.Type == "text" {
				resp.Blocks[i].Text = stripMinimaxLeak(b.Text)
			}
		}
	}
	if leaksToolCall(resp.Text()) {
		logToolCallLeak(r, requestBody, resp.Text(), len(resp.ToolUses()), resp.StopReason)
	}
	return applySalvage(resp, r.Tools)
}

// toolCallWrapper describes one non-standard tool-call envelope small models
// emit (observability: we want to know WHICH format the model used).
type toolCallWrapper struct{ open, close, origin string }

var toolCallWrappers = []toolCallWrapper{
	{"<tools>", "</tools>", "salvaged_tools_tag"},
	{"<tool_call>", "</tool_call>", "salvaged_tool_call_tag"},
	{"```json", "```", "salvaged_fenced_json"},
	{"```", "```", "salvaged_fenced_json"},
}

// salvageToolCall rescues a tool call a small model emitted as TEXT instead of
// the structured tool_calls envelope. Deliberately STRICT so prose about JSON
// never converts: after unwrapping, the entire text must be ONE JSON object of
// exactly {name, arguments} whose name matches a tool offered in THIS request.
func salvageToolCall(text string, tools []wire.ToolDef) (wire.Block, string, bool) {
	s := strings.TrimSpace(text)
	origin := "salvaged_bare_json"
	for _, w := range toolCallWrappers {
		if strings.HasPrefix(s, w.open) && strings.HasSuffix(s, w.close) && len(s) >= len(w.open)+len(w.close) {
			s = strings.TrimSpace(s[len(w.open) : len(s)-len(w.close)])
			origin = w.origin
			break
		}
	}
	if !strings.HasPrefix(s, "{") {
		// PREAMBLE+FENCE: models also emit "I'll use X:\n```json\n{call}\n```" —
		// real intent with explanatory prose. Extract the FIRST fenced block
		// anywhere in the text and try that. Still strict.
		if inner, ok := firstFencedBlock(text); ok {
			s = inner
			origin = "salvaged_fenced_json"
		} else {
			return wire.Block{}, "", false
		}
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	dec := json.NewDecoder(strings.NewReader(s))
	if dec.Decode(&call) != nil || dec.More() || call.Name == "" {
		return wire.Block{}, "", false
	}
	for _, t := range tools {
		if t.Name == call.Name {
			args := call.Arguments
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			return wire.Block{Type: "tool_use", ID: "salvaged_" + call.Name, Name: call.Name, Input: args}, origin, true
		}
	}
	return wire.Block{}, "", false
}

// firstFencedBlock extracts the JSON object inside the first ```…``` fence or
// <tool_call>…</tool_call> tag in text (preamble prose allowed before it).
func firstFencedBlock(text string) (string, bool) {
	for _, fence := range [][2]string{{"```json", "```"}, {"```", "```"}, {"<tool_call>", "</tool_call>"}, {"<tools>", "</tools>"}} {
		i := strings.Index(text, fence[0])
		if i < 0 {
			continue
		}
		rest := text[i+len(fence[0]):]
		j := strings.Index(rest, fence[1])
		if j < 0 {
			continue
		}
		inner := strings.TrimSpace(rest[:j])
		if strings.HasPrefix(inner, "{") {
			return inner, true
		}
	}
	return "", false
}

// minimaxLeakMarkers are MiniMax's tool-call special-token delimiters; when
// the parser misses them they leak into plain text. The FIRST one marks the
// start of a trailing leak envelope.
var minimaxLeakMarkers = []string{"]<]minimax[>[", "<minimax:tool_call", "<tool_call>"}

func isMinimaxModel(model string) bool { return strings.Contains(strings.ToLower(model), "minimax") }

// stripMinimaxLeak removes a TRAILING leaked tool-call envelope, preserving
// the real content before it.
func stripMinimaxLeak(s string) string {
	cut := -1
	for _, m := range minimaxLeakMarkers {
		if i := strings.Index(s, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return s
	}
	return strings.TrimRight(s[:cut], " \t\r\n")
}

// leaksToolCall reports whether response content contains tool-call markup
// that escaped the serving-side parser — the cue to capture the request.
func leaksToolCall(content string) bool {
	low := strings.ToLower(content)
	return strings.Contains(low, "<invoke") || strings.Contains(low, "<tool_call") ||
		strings.Contains(low, "</invoke") || strings.Contains(low, "minimax[>")
}

// logToolCallLeak emits a structured log entry (event=toolcall_leak) carrying
// the (bounded) request body + the leaked response, queryable in Cloud Logging
// by jsonPayload.event.
func logToolCallLeak(r wire.Request, requestBody []byte, leaked string, structuredCalls int, stop string) {
	if len(leaked) > 8000 {
		leaked = leaked[:8000] + "…(truncated)"
	}
	reqField := any(json.RawMessage(requestBody))
	if len(requestBody) > 24000 {
		reqField = string(requestBody[:24000]) + "…(truncated)"
	}
	entry, _ := json.Marshal(map[string]any{
		"severity":              "WARNING",
		"event":                 "toolcall_leak",
		"mode":                  r.Mode,
		"purpose":               r.Purpose,
		"structured_tool_calls": structuredCalls,
		"finish_reason":         stop,
		"request_bytes":         len(requestBody),
		"leaked_content":        leaked,
		"request":               reqField,
	})
	fmt.Println(string(entry))
}

// applySalvage rewrites a text-only response into a tool_use response when the
// text is a salvageable tool call. Structured tool_calls keep ToolOrigin
// "structured" (the primary path); a salvaged one tags its origin and logs —
// so salvage stays OBSERVABLE, never silently the main contract.
func applySalvage(resp wire.Response, tools []wire.ToolDef) wire.Response {
	if len(resp.ToolUses()) > 0 {
		resp.ToolOrigin = "structured_openai"
		return resp
	}
	if len(tools) == 0 {
		return resp
	}
	if b, origin, ok := salvageToolCall(resp.Text(), tools); ok {
		resp.Blocks = []wire.Block{b}
		resp.StopReason = "tool_use"
		resp.ToolOrigin = origin
		log.Printf("tool_call_origin=%s tool=%s (serving-side parser missed structured tool_calls)", origin, b.Name)
	}
	return resp
}
