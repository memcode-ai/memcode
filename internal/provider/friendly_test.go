package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// FriendlyError turns the SDKs' raw request-line + JSON dumps into a status
// lead plus the provider's own message.
func TestFriendlyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"anthropic sdk 429",
			errors.New(`POST "https://api.anthropic.com/v1/messages": 429 Too Many Requests (Request-ID: req_011) {"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_011"}`),
			"Rate limited by the provider (HTTP 429): Number of request tokens has exceeded your per-minute rate limit",
		},
		{
			"openai sdk 400",
			errors.New(`POST "https://api.openai.com/v1/chat/completions": 400 Bad Request {
  "error": {
    "message": "Invalid 'messages[3].content': string too long.",
    "type": "invalid_request_error"
  }
}`),
			"The provider rejected the request (HTTP 400): Invalid 'messages[3].content': string too long.",
		},
		{
			"anthropic overloaded",
			errors.New(`POST "https://api.anthropic.com/v1/messages": 529  (Request-ID: req_011) {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
			"The provider is overloaded — try again shortly (HTTP 529): Overloaded",
		},
		{
			"compat http text",
			errors.New(`copilot: http 403: {"message":"You do not have access to this model"}`),
			"The provider denied access (HTTP 403): You do not have access to this model",
		},
		{
			"gemini sdk",
			errors.New(`Error 429, Message: Resource has been exhausted (e.g. check quota)., Status: RESOURCE_EXHAUSTED, Details: []`),
			"Rate limited by the provider (HTTP 429): Resource has been exhausted (e.g. check quota).",
		},
		{
			"string error field",
			errors.New(`gateway: {"error":"model not available on your plan"}`),
			"Model not available on your plan",
		},
		{
			"json number is not a status",
			errors.New(`bad tool input {"input": 500}`),
			`bad tool input {"input": 500}`,
		},
		{
			"plain error passes through",
			errors.New("unexpected end of stream"),
			"unexpected end of stream",
		},
		{
			"deadline",
			fmt.Errorf("stream: %w", context.DeadlineExceeded),
			"The request timed out before the provider answered — try again",
		},
	}
	for _, c := range cases {
		if got := FriendlyError(c.err); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// The lane-exhausted line keeps its plain sentence and gains the provider's
// own message from the wrapped error.
func TestFriendlyErrorLaneExhausted(t *testing.T) {
	exh := &ErrLaneExhausted{
		Lane:   LaneInfo{Vendor: "anthropic", Name: "claude-sub", Kind: "sub"},
		Status: 429,
		Err:    errors.New(`POST "https://api.anthropic.com/v1/messages": 429 Too Many Requests {"type":"error","error":{"message":"5-hour limit reached"}}`),
	}
	want := "your claude subscription hit its usage limit: 5-hour limit reached"
	if got := FriendlyError(fmt.Errorf("turn: %w", exh)); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Bounded rune-safely and generously — the old 120-byte cut split UTF-8 and
// dropped the provider message.
func TestFriendlyErrorBound(t *testing.T) {
	long := strings.Repeat("é", 1000)
	got := FriendlyError(errors.New(long))
	if r := []rune(got); len(r) != friendlyMax+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("len = %d runes, want %d with ellipsis", len(r), friendlyMax+1)
	}
}
