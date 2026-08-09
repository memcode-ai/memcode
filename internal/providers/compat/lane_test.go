package compat

// Lane behavior tests ported with the engine consolidation (salvage net,
// model-conditional reasoning effort, lane error classification).

import (
	"errors"
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// TestReasoningEffortMapping locks the Effort→reasoning_effort policy: ordinary turns reason at
// the model's HIGH tier, hard turns (EffortHigh) at MAX, tool-less utility calls and non-reasoning
// models get nothing (so GLM no longer silently defaults to Max on every turn).
func TestReasoningEffortMapping(t *testing.T) {
	glm := "accounts/fireworks/models/glm-5p2"
	cases := []struct {
		name     string
		model    string
		eff      wire.Effort
		hasTools bool
		want     string
	}{
		{"ordinary agentic → high", glm, wire.EffortOff, true, "high"},
		{"medium agentic → high", glm, wire.EffortMedium, true, "high"},
		{"hard agentic → max", glm, wire.EffortHigh, true, "max"},
		{"tool-less GLM, no effort → none (suppress default thinking)", glm, wire.EffortOff, false, "none"},
		{"tool-less Kimi K3, no effort → none", "accounts/fireworks/models/kimi-k3", wire.EffortOff, false, "none"},
		{"tool-less GLM WITH effort → omit (let it think)", glm, wire.EffortHigh, false, ""},
		{"tool-less gpt-oss → omit (rejects none)", "accounts/fireworks/models/gpt-oss-120b", wire.EffortOff, false, ""},
		{"non-reasoning model → omit", "accounts/fireworks/models/llama-v3", wire.EffortHigh, true, ""},
	}
	for _, c := range cases {
		if got := laneReasoningEffort(c.model, c.eff, c.hasTools); got != c.want {
			t.Errorf("%s: laneReasoningEffort(%q,%v,%v)=%q want %q", c.name, c.model, c.eff, c.hasTools, got, c.want)
		}
	}
}

// The lane error contract must flag an overflow 400 as an overflow request
// error, and leave a 5xx a generic error.
func TestLaneErrorOverflowFlag(t *testing.T) {
	tr := New(Config{BaseURL: "http://x/v1", Lane: true})
	err := tr.mapError(400, []byte(`{"error":{"message":"This model's maximum context length is 65536 tokens"}}`))
	var ve *LaneRequestError
	if !errors.As(err, &ve) || !ve.Overflow {
		t.Fatalf("a 400 context-overflow must be an overflow-flagged LaneRequestError, got %v", err)
	}
	if generic := tr.mapError(500, []byte("boom")); errors.As(generic, &ve) {
		t.Error("a 5xx is infra, never a LaneRequestError")
	}
}
