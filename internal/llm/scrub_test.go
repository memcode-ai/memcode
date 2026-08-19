package llm

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// Thinking blocks must never reach a non-Anthropic serving path, and shared
// history slices must never be mutated by the scrub.
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
		t.Fatal("anthropic serving must keep thinking blocks")
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
