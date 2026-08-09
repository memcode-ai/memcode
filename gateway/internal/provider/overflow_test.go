package provider

import (
	"errors"
	"fmt"
	"testing"
)

// The overflow classifier is load-bearing: it decides whether the CLI gets a
// compact-and-retry signal (413/context_overflow) or a plain 502. It must catch
// BOTH backends' phrasings and must NOT fire on unrelated errors.
func TestIsContextOverflowPhrasings(t *testing.T) {
	overflow := []string{
		"This model's maximum context length is 65536 tokens",
		"the input is longer than the maximum context length",
		"please reduce the length of the messages",
		"prompt is too long: 1050000 tokens > 1000000 maximum",
		"input length exceeds the maximum",
		"context_length_exceeded",
	}
	for _, m := range overflow {
		if !isContextOverflow(m) {
			t.Errorf("should classify as overflow: %q", m)
		}
	}
	notOverflow := []string{
		"rate limit exceeded",
		"internal server error",
		"invalid api key",
		"tool input failed to parse",
		"",
	}
	for _, m := range notOverflow {
		if isContextOverflow(m) {
			t.Errorf("should NOT classify as overflow: %q", m)
		}
	}
}

func TestIsContextOverflowTypedErrors(t *testing.T) {
	// Anthropic-side overflow.
	if !IsContextOverflow(&ContextOverflowError{Backend: "anthropic", Message: "prompt is too long"}) {
		t.Error("ContextOverflowError must be recognized")
	}
	// vLLM request error flagged Overflow.
	if !IsContextOverflow(&LaneRequestError{Status: 400, Message: "maximum context length", Overflow: true}) {
		t.Error("overflow-flagged LaneRequestError must be recognized")
	}
	// vLLM request error NOT an overflow (a different 400) must not trip it.
	if IsContextOverflow(&LaneRequestError{Status: 400, Message: "bad request", Overflow: false}) {
		t.Error("a non-overflow request error must not be classified as overflow")
	}
	// Wrapped error still matches (errors.As unwraps).
	wrapped := fmt.Errorf("serving turn: %w", &ContextOverflowError{Backend: "vllm", Message: "too long"})
	if !IsContextOverflow(wrapped) {
		t.Error("a wrapped overflow error must still be recognized")
	}
	// Unrelated errors don't match.
	if IsContextOverflow(errors.New("connection refused")) {
		t.Error("an unrelated error must not be classified as overflow")
	}
}
