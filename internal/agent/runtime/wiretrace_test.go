package runtime

import (
	"errors"
	"testing"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/wire"
)

// TestWireRecord: the trace captures the three things today's bugs each needed — the tools
// that were sent, the output budget, and the block types the model returned (so a
// thinking-only, no-tool_use turn is legible at a glance).
func TestWireRecord(t *testing.T) {
	req := wire.Request{
		Mode: "chat", Effort: wire.EffortHigh, MaxTokens: 4096,
		Messages: []wire.Message{{Role: "user"}},
		Tools:    []wire.ToolDef{{Name: "edit_file"}, {Name: "bash"}},
	}
	// The pathological turn: thinking-only, no tool_use, cut off at the cap.
	resp := wire.Response{
		Backend: "cheap", Model: "glm-5p2", StopReason: "max_tokens", OutputTokens: 4096,
		Blocks: []wire.Block{{Type: "thinking", Thinking: "…"}},
	}

	rec := wireRecord(llm.MainLoop, req, resp, nil)

	if got := rec["tools"].([]string); len(got) != 2 || got[0] != "edit_file" {
		t.Fatalf("tools not captured: %v", rec["tools"])
	}
	if rec["max_tokens"].(int) != 4096 {
		t.Fatalf("max_tokens not captured: %v", rec["max_tokens"])
	}
	if rec["stop"] != "max_tokens" {
		t.Fatalf("stop_reason not captured: %v", rec["stop"])
	}
	blocks := rec["blocks"].(map[string]int)
	if blocks["tool_use"] != 0 || blocks["thinking"] != 1 {
		t.Fatalf("block counts wrong (want 0 tool_use, 1 thinking): %v", blocks)
	}
}

// TestWireRecordError: on a failed call the error is captured and response fields are omitted.
func TestWireRecordError(t *testing.T) {
	rec := wireRecord(llm.MainLoop, wire.Request{Mode: "chat"}, wire.Response{}, errors.New("boom"))
	if rec["err"] != "boom" {
		t.Fatalf("err not captured: %v", rec["err"])
	}
	if _, ok := rec["blocks"]; ok {
		t.Fatal("blocks should be absent on error")
	}
}
