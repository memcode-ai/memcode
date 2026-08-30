package runtime

import (
	"encoding/json"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func TestSuspensionRoundTripPreservesReasoningAndTool(t *testing.T) {
	root := t.TempDir()
	assistant := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "thinking", Thinking: "reason", Signature: "sig"}, {Type: "tool_use", ID: "tool-1", Name: "ask_user", Input: json.RawMessage(`{"question":"continue?"}`)}}}
	s := Suspension{SessionID: "session-1", InteractionID: "interaction-1", ToolUseID: "tool-1", ToolName: "ask_user", ToolInput: assistant.Blocks[1].Input, Assistant: assistant}
	if err := SaveSuspension(root, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSuspension(root, "session-1", "interaction-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Assistant.Blocks[0].Signature != "sig" || got.ToolUseID != "tool-1" {
		t.Fatalf("suspension=%+v", got)
	}
	msgs, err := ResolveSuspension(root, got, wire.Block{Type: "tool_result", ToolUseID: "tool-1", Content: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Blocks[0].ToolUseID != "tool-1" {
		t.Fatalf("messages=%+v", msgs)
	}
	if _, err := LoadSuspension(root, "session-1", "interaction-1"); err == nil {
		t.Fatal("resolved suspension loaded again")
	}
}

func TestSuspensionRejectsMixedBatchAndMismatchedResult(t *testing.T) {
	root := t.TempDir()
	mixed := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "a"}, {Type: "tool_use", ID: "b"}}}
	if err := SaveSuspension(root, Suspension{SessionID: "s", InteractionID: "i", ToolUseID: "a", Assistant: mixed}); err == nil {
		t.Fatal("mixed tool batch accepted")
	}
	single := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "a", Name: "approval"}}}
	s := Suspension{SessionID: "s", InteractionID: "i2", ToolUseID: "a", Assistant: single}
	if err := SaveSuspension(root, s); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSuspension(root, "s", "i2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSuspension(root, loaded, wire.Block{Type: "tool_result", ToolUseID: "wrong"}); err == nil {
		t.Fatal("mismatched result accepted")
	}
}
