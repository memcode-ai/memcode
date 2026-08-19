package llm

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Thinking blocks foreign to the serving vendor must never reach it, blocks
// NATIVE to it must survive, and shared history slices must never be mutated
// by the scrub.
func TestScrubForeignThinking(t *testing.T) {
	history := []wire.Message{
		{Role: "user", Blocks: []wire.Block{wire.TextBlock("q")}},
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "thinking", Thinking: "hmm", Signature: "sig"},
			wire.TextBlock("a"),
		}},
	}
	req := wire.Request{Messages: append([]wire.Message(nil), history...)}

	// Anthropic serving: untouched.
	scrubForeignThinking(&req, "sonnet")
	if len(req.Messages[1].Blocks) != 2 {
		t.Fatal("anthropic serving must keep anthropic thinking blocks")
	}

	// Fireworks serving: thinking dropped, text kept, ORIGINAL history intact.
	scrubForeignThinking(&req, "kimi-k3")
	if len(req.Messages[1].Blocks) != 1 || req.Messages[1].Blocks[0].Type != "text" {
		t.Fatalf("scrubbed blocks = %+v", req.Messages[1].Blocks)
	}
	if len(history[1].Blocks) != 2 {
		t.Fatal("scrub mutated the shared history slice")
	}
}

// anthropic→openai lane switch: the Anthropic thinking block is foreign on the
// openai serve and drops, but OpenAI's own rs_* reasoning item survives.
func TestScrubAnthropicToOpenAIKeepsNativeReasoning(t *testing.T) {
	history := []wire.Message{
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "thinking", Thinking: "claude cot", Signature: "anth-sig"},
			{Type: "thinking", ID: "rs_abc123", Signature: "enc"},
			wire.TextBlock("a"),
		}},
	}
	req := wire.Request{Messages: append([]wire.Message(nil), history...)}

	scrubForeignThinking(&req, "terra")
	blocks := req.Messages[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("openai serving blocks = %+v, want [rs_ thinking, text]", blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].ID != "rs_abc123" {
		t.Fatalf("openai serving must keep its native rs_* reasoning: %+v", blocks[0])
	}
	if blocks[1].Type != "text" {
		t.Fatalf("text block must survive: %+v", blocks[1])
	}
	if len(history[0].Blocks) != 3 {
		t.Fatal("scrub mutated the shared history slice")
	}
}

// openai→anthropic lane switch: the rs_* reasoning item is foreign on the
// anthropic serve and drops, but Anthropic's own thinking (and
// redacted_thinking) survive.
func TestScrubOpenAIToAnthropicKeepsNativeThinking(t *testing.T) {
	history := []wire.Message{
		{Role: "assistant", Blocks: []wire.Block{
			{Type: "thinking", ID: "rs_abc123", Signature: "enc"},
			{Type: "thinking", Thinking: "claude cot", Signature: "anth-sig"},
			{Type: "redacted_thinking", Data: "opaque"},
			wire.TextBlock("a"),
		}},
	}
	req := wire.Request{Messages: append([]wire.Message(nil), history...)}

	scrubForeignThinking(&req, "sonnet")
	blocks := req.Messages[0].Blocks
	if len(blocks) != 3 {
		t.Fatalf("anthropic serving blocks = %+v, want [thinking, redacted_thinking, text]", blocks)
	}
	if blocks[0].Type != "thinking" || blocks[0].Signature != "anth-sig" {
		t.Fatalf("anthropic serving must keep its native thinking: %+v", blocks[0])
	}
	if blocks[1].Type != "redacted_thinking" || blocks[1].Data != "opaque" {
		t.Fatalf("redacted_thinking must survive an anthropic serve: %+v", blocks[1])
	}
	if len(history[0].Blocks) != 4 {
		t.Fatal("scrub mutated the shared history slice")
	}
}
