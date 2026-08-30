package continuation

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

func TestRoundTripPreservesReasoningAndTool(t *testing.T) {
	dir := t.TempDir()
	assistant := wire.Message{Role: "assistant", Blocks: []wire.Block{
		{Type: "thinking", Thinking: "reason", Signature: "sig"},
		{Type: "tool_use", ID: "tool-1", Name: "ask_user", Input: json.RawMessage(`{"question":"continue?"}`)},
	}}
	s := Suspension{SessionID: "session-1", InteractionID: "interaction-1", ToolUseID: "tool-1", ToolName: "ask_user", ToolInput: assistant.Blocks[1].Input, Assistant: assistant}
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "interaction-1")
	if err != nil {
		t.Fatal(err)
	}
	// Thinking signature must survive the round trip — dropping it invalidates
	// the assistant turn when it is replayed to the model.
	if got.Assistant.Blocks[0].Signature != "sig" || got.ToolUseID != "tool-1" {
		t.Fatalf("suspension=%+v", got)
	}
	msgs, err := Resolve(dir, got, wire.Block{Type: "tool_result", ToolUseID: "tool-1", Content: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].Blocks[0].ToolUseID != "tool-1" {
		t.Fatalf("messages=%+v", msgs)
	}
	if _, err := Load(dir, "interaction-1"); err == nil {
		t.Fatal("resolved suspension loaded again — a second resume would re-run real side effects")
	}
}

func TestRejectsMixedBatchAndMismatchedResult(t *testing.T) {
	dir := t.TempDir()
	mixed := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "a"}, {Type: "tool_use", ID: "b"}}}
	if err := Save(dir, Suspension{InteractionID: "i", ToolUseID: "a", Assistant: mixed}); err == nil {
		t.Fatal("mixed tool batch accepted — a sibling call would be dropped or re-run on resume")
	}
	single := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "a", Name: "approval"}}}
	if err := Save(dir, Suspension{InteractionID: "i2", ToolUseID: "a", Assistant: single}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir, "i2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(dir, loaded, wire.Block{Type: "tool_result", ToolUseID: "wrong"}); err == nil {
		t.Fatal("mismatched result accepted — resume would answer the wrong question")
	}
}

// The unattended-executive case: no transcript of its own, so the continuation
// carries the whole conversation and Resolve hands all of it back.
func TestFullTranscriptCarriedForTranscriptlessCaller(t *testing.T) {
	dir := t.TempDir()
	assistant := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t1", Name: "ask_user"}}}
	msgs := []wire.Message{
		{Role: "user", Blocks: []wire.Block{wire.TextBlock("advance the objective")}},
		{Role: "assistant", Blocks: []wire.Block{wire.TextBlock("checking")}},
		assistant,
	}
	if err := Save(dir, Suspension{InteractionID: "i", ToolUseID: "t1", Assistant: assistant, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir, "i")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Resolve(dir, loaded, wire.Block{Type: "tool_result", ToolUseID: "t1", Content: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	// Whole transcript + the answer, with nothing duplicated.
	if len(out) != len(msgs)+1 {
		t.Fatalf("expected %d messages, got %d: %+v", len(msgs)+1, len(out), out)
	}
	if out[len(out)-1].Blocks[0].ToolUseID != "t1" {
		t.Fatalf("answer not appended: %+v", out)
	}
}

func TestMarkResolvedBlocksReload(t *testing.T) {
	dir := t.TempDir()
	assistant := wire.Message{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t1", Name: "ask_user"}}}
	if err := Save(dir, Suspension{InteractionID: "i", ToolUseID: "t1", Assistant: assistant}); err != nil {
		t.Fatal(err)
	}
	if err := MarkResolved(dir, "i"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "i"); err == nil {
		t.Fatal("expected a marked-resolved continuation to refuse loading")
	}
}

func TestSessionDirLayout(t *testing.T) {
	got := SessionDir("/repo", "sess_abc")
	want := filepath.Join("/repo", ".memcode", "sessions", "sess_abc", "continuations")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
