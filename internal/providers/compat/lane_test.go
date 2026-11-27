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
	glm := "accounts/fireworks/models/glm-5p3"
	kimi := "accounts/fireworks/models/kimi-k3p1-pro"
	for _, c := range []struct {
		name  string
		model string
		eff   wire.Effort
		want  string
	}{
		{"resolved level passes through", glm, wire.EffortMax, "max"},
		{"high passes through", glm, wire.EffortHigh, "high"},
		{"low passes through", kimi, wire.EffortLow, "low"},
		{"default omits the field", glm, wire.EffortDefault, ""},
		{"off on GLM sends its floor (GLM rejects none)", glm, wire.EffortOff, "high"},
		{"off on Kimi sends none (it thinks when omitted)", kimi, wire.EffortOff, "none"},
		{"non-reasoning model → omit", "accounts/fireworks/models/llama-v3", wire.EffortHigh, ""},
	} {
		if got := laneReasoningEffort(c.model, c.eff); got != c.want {
			t.Errorf("%s: laneReasoningEffort(%q,%v)=%q want %q", c.name, c.model, c.eff, got, c.want)
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
