package runtime

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// TestSetPinStripsThinkingBlocksFromLiveChat: a mid-session /model switch (SetPin)
// must drop thinking/redacted_thinking blocks from the live chat history. Those
// blocks are provider-specific — Anthropic validates the signature it issued, and a
// different model can't vouch for them. Replaying foreign thinking produces
// "thinking blocks in the latest assistant message cannot be modified" (hard 400).
// Text and tool blocks are provider-neutral and must survive.
func TestSetPinStripsThinkingBlocksFromLiveChat(t *testing.T) {
	s := &Session{
		liveChat: &ChatState{messages: []wire.Message{
			{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "do the thing"}}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "thinking", Thinking: "reasoning here", Signature: "sig_old_model"},
				{Type: "text", Text: "ok"},
				{Type: "tool_use", ID: "tu_1", Name: "read_file", Input: []byte(`{}`)},
			}},
			{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "tu_1", Content: "body"}}},
			{Role: "assistant", Blocks: []wire.Block{
				{Type: "redacted_thinking", Data: "opaque_blob"},
				{Type: "text", Text: "done"},
			}},
		}},
	}

	s.SetPin("sonnet", 0)

	msgs := s.liveChat.messages
	// No thinking/redacted_thinking blocks survive in any assistant message.
	for i, m := range msgs {
		for _, b := range m.Blocks {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				t.Fatalf("message %d still has a %q block after SetPin: %+v", i, b.Type, m.Blocks)
			}
		}
	}
	// Text and tool blocks are provider-neutral and must survive.
	if len(msgs[1].Blocks) != 2 || msgs[1].Blocks[0].Type != "text" || msgs[1].Blocks[1].Type != "tool_use" {
		t.Fatalf("assistant text+tool blocks must survive, got %+v", msgs[1].Blocks)
	}
	if len(msgs[3].Blocks) != 1 || msgs[3].Blocks[0].Type != "text" {
		t.Fatalf("final assistant text must survive, got %+v", msgs[3].Blocks)
	}
}

// TestSetPinStripsThinkingNoOpWithoutLiveChat: SetPin on a headless session (no
// interactive ChatState attached) must not panic — explore/Answer build ephemeral
// histories and never switch models mid-flight.
func TestSetPinStripsThinkingNoOpWithoutLiveChat(t *testing.T) {
	s := &Session{}       // no liveChat
	s.SetPin("sonnet", 0) // must not panic
	if s.Pin() != "sonnet" {
		t.Fatalf("pin = %q, want sonnet", s.Pin())
	}
}

// TestStripToolBlocks: a plan thread built on one model (with tool_use/tool_result/thinking
// blocks) is reduced to a clean, role-alternating natural-language transcript before it's
// handed to a different reviewer model — so another backend's tool plumbing never reaches
// Anthropic (which rejects an unpaired tool_use with a 400).
func TestStripToolBlocks(t *testing.T) {
	in := []wire.Message{
		{Role: "user", Blocks: []wire.Block{{Type: "text", Text: "do the task"}}},
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "thinking", Thinking: "scratch"},
			{Type: "text", Text: "let me look"},
			{Type: "tool_use", ID: "chatcmpl-tool-1", Name: "read"},
		}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "chatcmpl-tool-1", Content: "file body"}}},
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "tool_use", ID: "chatcmpl-tool-2", Name: "grep"}, // tool-only turn → drops to empty
		}},
		{Role: "user", Blocks: []wire.Block{{Type: "tool_result", ToolUseID: "chatcmpl-tool-2", Content: "match"}}},
		{Role: "assistant", Blocks: []wire.Block{{Type: "text", Text: "here is the plan"}}},
	}

	out := stripToolBlocks(in)

	// No tool/thinking blocks survive.
	for _, m := range out {
		for _, b := range m.Blocks {
			if b.Type == "tool_use" || b.Type == "tool_result" || b.Type == "thinking" || b.Type == "redacted_thinking" {
				t.Fatalf("stripToolBlocks left a %q block: %+v", b.Type, out)
			}
		}
	}
	// Roles must alternate (the empty tool turns collapsed; consecutive assistants merged).
	for i := 1; i < len(out); i++ {
		if out[i].Role == out[i-1].Role {
			t.Fatalf("roles do not alternate at %d: %+v", i, out)
		}
	}
	// Expect: user(task) → assistant("let me look" + "here is the plan").
	if len(out) != 2 || out[0].Role != "user" || out[1].Role != "assistant" {
		t.Fatalf("want [user, assistant], got %d messages: %+v", len(out), out)
	}
	if got := len(out[1].Blocks); got != 2 {
		t.Fatalf("want the two assistant text blocks merged into one turn, got %d: %+v", got, out[1].Blocks)
	}
}
