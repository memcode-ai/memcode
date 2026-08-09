package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// TestThinkingWireShape: the abstract Effort maps to adaptive thinking + top-level
// output_config.effort on the latest Opus/Sonnet, and is OMITTED everywhere else
// (cheap tiers, unknown models, EffortOff) so we never risk a 400.
func TestThinkingWireShape(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		effort       wire.Effort
		wantThinking bool
		wantEffort   string
	}{
		{"opus48 medium", "claude-opus-5", wire.EffortMedium, true, "medium"},
		{"opus48 high", "claude-opus-5[1m]", wire.EffortHigh, true, "high"},
		{"sonnet5 low", "claude-sonnet-5", wire.EffortLow, true, "low"},
		{"opus48 off", "claude-opus-5", wire.EffortOff, false, ""},
		{"haiku stays off", "claude-haiku-4-5", wire.EffortHigh, false, ""},
		{"unknown model off", "some-other-model", wire.EffortHigh, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := buildWire(wire.Request{Model: c.model, Effort: c.effort, Messages: []wire.Message{
				{Role: "user", Blocks: []wire.Block{wire.TextBlock("hi")}},
			}}, 4096, false)
			if (w.Thinking != nil) != c.wantThinking {
				t.Fatalf("thinking present=%v, want %v", w.Thinking != nil, c.wantThinking)
			}
			if c.wantThinking {
				if w.Thinking.Type != "adaptive" {
					t.Errorf("thinking.type = %q, want adaptive", w.Thinking.Type)
				}
				if w.OutputConfig == nil || w.OutputConfig.Effort != c.wantEffort {
					t.Errorf("output_config.effort = %+v, want %q", w.OutputConfig, c.wantEffort)
				}
			} else if w.OutputConfig != nil {
				t.Errorf("output_config should be nil when thinking is off, got %+v", w.OutputConfig)
			}

			// Verify the JSON: effort lives in top-level output_config, NOT in thinking,
			// and budget_tokens never appears (adaptive-only).
			b, _ := json.Marshal(w)
			js := string(b)
			if strings.Contains(js, "budget_tokens") {
				t.Errorf("must not send budget_tokens (adaptive-only): %s", js)
			}
			if c.wantThinking && !strings.Contains(js, `"output_config":{"effort":`) {
				t.Errorf("effort must be under top-level output_config: %s", js)
			}
		})
	}
}

// TestThinkingBlockRoundTrips is the correctness guard for tool-use turns: a
// thinking block returned by the API must serialize back UNMODIFIED (the signature
// is load-bearing — the API rejects the next turn otherwise), even when the
// thinking text is empty (omitted display).
func TestThinkingBlockRoundTrips(t *testing.T) {
	orig := wire.Block{Type: "thinking", Thinking: "let me reason...", Signature: "abc123sig"}
	b, _ := json.Marshal(orig)
	var got wire.Block
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "thinking" || got.Thinking != orig.Thinking || got.Signature != orig.Signature {
		t.Errorf("thinking block did not round-trip: %+v", got)
	}

	// Empty thinking text (adaptive thinking returns a signature-only block): the
	// "thinking" field MUST still be present — Anthropic rejects a thinking block
	// without it on the next tool-use turn ("thinking.thinking: Field required").
	// (This previously asserted the opposite; the live 400 proved it wrong.)
	omitted := wire.Block{Type: "thinking", Signature: "sigonly"}
	b2, _ := json.Marshal(omitted)
	if !strings.Contains(string(b2), `"signature":"sigonly"`) {
		t.Errorf("signature must survive even with empty thinking: %s", b2)
	}
	if !strings.Contains(string(b2), `"thinking":""`) {
		t.Errorf("empty thinking must still send the field, not omit it: %s", b2)
	}
	// A non-thinking block must NOT carry a stray thinking field.
	if tb, _ := json.Marshal(wire.Block{Type: "text", Text: "hi"}); strings.Contains(string(tb), "thinking") {
		t.Errorf("text block must not carry a thinking field: %s", tb)
	}
}
