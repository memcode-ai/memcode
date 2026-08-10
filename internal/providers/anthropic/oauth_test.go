package anthropic

import (
	"testing"

	"github.com/memcode-ai/memcode/internal/wire"
)

// The compatibility mode must be gated on the credential: a subscription OAuth
// token turns it on, a normal console key never does. This is the guard that
// keeps the Claude Code identity off own-key traffic (the hermes #7366 bug).
func TestIsOAuthTokenGating(t *testing.T) {
	on := []string{"cc-abc123", "eyJhbGciOi.payload.sig", "sk-ant-oat01-xyz"}
	off := []string{"sk-ant-api03-realkey", "", "random"}
	for _, k := range on {
		if !isOAuthToken(k) {
			t.Errorf("%q should enable compatibility mode", k)
		}
	}
	for _, k := range off {
		if isOAuthToken(k) {
			t.Errorf("%q must NOT enable compatibility mode (own-key stays clean)", k)
		}
	}
}

// NewAnthropic wires the mode from the token shape.
func TestNewAnthropicModeFromToken(t *testing.T) {
	if NewAnthropic("sk-ant-api03-key").oauth {
		t.Error("a console API key must not enable oauth mode")
	}
	if !NewAnthropic("cc-oauth-token").oauth {
		t.Error("an OAuth token must enable oauth mode")
	}
}

func TestToOAuthToolName(t *testing.T) {
	cases := map[string]string{
		"bash":                 "mcp__bash",
		"read_file":            "mcp__read_file",
		"mcp_single":           "mcp__single",
		"mcp__supabase__query": "mcp__supabase__query", // already prefixed — untouched
	}
	for in, want := range cases {
		if got := toOAuthToolName(in); got != want {
			t.Errorf("toOAuthToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// Encoding renames tools + history to mcp__ form and prepends the identity;
// decoding restores the real names via the reverse map — including the tricky
// case where an already-mcp__ tool must NOT be stripped.
func TestOAuthEncodeDecodeRoundTrip(t *testing.T) {
	r := wire.Request{
		System: "You are memcode. Use .memcode/ for state.",
		Tools: []wire.ToolDef{
			{Name: "bash"},
			{Name: "mcp__supabase__query"},
		},
		ToolChoice: "bash",
		Messages: []wire.Message{
			{Role: "assistant", Blocks: []wire.Block{{Type: "tool_use", ID: "t1", Name: "bash"}}},
		},
	}
	enc, rev := oauthEncodeRequest(r)

	// System is preserved VERBATIM: the identity is NOT fused in here — the OAuth
	// filter needs system[0] to be exactly the identity string, so buildWire emits
	// it as its own leading block instead of concatenating it onto the doctrine.
	if enc.System != "You are memcode. Use .memcode/ for state." {
		t.Errorf("system must be preserved verbatim (identity is a separate buildWire block), got %q", enc.System)
	}
	// Tool defs renamed; already-prefixed one untouched.
	if enc.Tools[0].Name != "mcp__bash" || enc.Tools[1].Name != "mcp__supabase__query" {
		t.Errorf("tool defs wrong: %q, %q", enc.Tools[0].Name, enc.Tools[1].Name)
	}
	// Forced tool choice renamed to match the definition, or the API 400s.
	if enc.ToolChoice != "mcp__bash" {
		t.Errorf("tool_choice not renamed: %q, want mcp__bash", enc.ToolChoice)
	}
	// History tool_use renamed.
	if enc.Messages[0].Blocks[0].Name != "mcp__bash" {
		t.Errorf("history tool_use not renamed: %q", enc.Messages[0].Blocks[0].Name)
	}
	// The caller's request is untouched.
	if r.Tools[0].Name != "bash" || r.System != "You are memcode. Use .memcode/ for state." {
		t.Error("oauthEncodeRequest mutated the caller's request")
	}

	// Decode a response: mcp__bash restores to bash; mcp__supabase__query stays.
	resp := wire.Response{Blocks: []wire.Block{
		{Type: "tool_use", Name: "mcp__bash"},
		{Type: "tool_use", Name: "mcp__supabase__query"},
	}}
	oauthDecodeResponse(&resp, rev)
	if resp.Blocks[0].Name != "bash" {
		t.Errorf("mcp__bash should decode to bash, got %q", resp.Blocks[0].Name)
	}
	if resp.Blocks[1].Name != "mcp__supabase__query" {
		t.Errorf("an already-prefixed tool must NOT be stripped, got %q", resp.Blocks[1].Name)
	}
}
