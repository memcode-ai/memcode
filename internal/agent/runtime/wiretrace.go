package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/memcode-ai/memcode/internal/config"
	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// Wire trace: env-gated end-to-end capture of every model call, the layer the human
// transcript (events.jsonl) deliberately omits. When MEMCODE_TRACE is set, s.complete
// appends ONE json line per call to .memcode/sessions/<id>/wire.jsonl recording the
// REQUEST shape (purpose/mode/effort/max_tokens/tools-sent/msg-count/hint) and the
// RESPONSE shape (backend/model/stop_reason/block-type counts/output tokens/err). It's
// the difference between "seems like it doesn't have the tools" and a one-line answer:
//
//	grep -c tool_use .memcode/sessions/*/wire.jsonl   # did it ever emit a tool call?
//
// Off by default — zero cost when unset (the env is read once at process start).
var wireTraceEnabled = os.Getenv("MEMCODE_TRACE") != ""

// wireRecord is the pure shape of one traced call — built without touching the filesystem
// so it's unit-testable. It answers the three questions today's bugs each needed: were the
// tools in the request, what was the budget, and did the response come back as tool_use vs
// text vs thinking (and why it stopped).
func wireRecord(purpose llm.Purpose, req wire.Request, resp wire.Response, err error) map[string]any {
	rec := map[string]any{
		"purpose":    string(purpose),
		"mode":       req.Mode,
		"effort":     string(req.Effort),
		"max_tokens": req.MaxTokens,
		"tools":      toolNames(req.Tools),
		"msgs":       len(req.Messages),
	}
	if err != nil {
		rec["err"] = err.Error()
		return rec
	}
	text, toolUse, thinking := 0, 0, 0
	for _, b := range resp.Blocks {
		switch b.Type {
		case "text":
			text++
		case "tool_use":
			toolUse++
		case "thinking", "redacted_thinking":
			thinking++
		}
	}
	rec["backend"] = resp.Backend
	rec["model"] = resp.Model
	rec["stop"] = resp.StopReason
	rec["blocks"] = map[string]int{"text": text, "tool_use": toolUse, "thinking": thinking}
	rec["out_tokens"] = resp.OutputTokens
	return rec
}

// traceWire appends one wireRecord to the session's wire.jsonl. Best-effort: gated on the
// env flag and a real session id (sub-agents/forks have none → skipped), and silent on any
// I/O error — a diagnostic trace must never affect the turn.
func (s *Session) traceWire(purpose llm.Purpose, req wire.Request, resp wire.Response, err error) {
	if !wireTraceEnabled || s.sessionID == "" {
		return
	}
	line, merr := json.Marshal(wireRecord(purpose, req, resp, err))
	if merr != nil {
		return
	}
	dir := filepath.Join(s.root, config.DirName, "sessions", s.sessionID)
	_ = os.MkdirAll(dir, 0o755)
	f, ferr := os.OpenFile(filepath.Join(dir, "wire.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if ferr != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

func toolNames(tools []wire.ToolDef) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
