package copilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A classic PAT is the one token type the Copilot exchange rejects — fail it
// early with a fixable message instead of a 404 mid-turn.
func TestValidTokenTypeRejectsClassicPAT(t *testing.T) {
	if err := validTokenType("ghp_classic"); err == nil {
		t.Fatal("a ghp_ classic PAT must be rejected")
	}
	for _, ok := range []string{"gho_oauth", "github_pat_fine", "ghu_app"} {
		if err := validTokenType(ok); err != nil {
			t.Errorf("%s must be accepted, got %v", ok, err)
		}
	}
}

// The exchange must send `Authorization: token <raw>`, read the minted token +
// endpoint, and cache it so a second call skips the network.
func TestExchangeAndCache(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // isolate the on-disk cache
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "token raw-gh-token" {
			t.Errorf("exchange Authorization = %q, want token raw-gh-token", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "copilot-minted",
			"expires_at": 9999999999,
			"endpoints":  map[string]string{"api": "https://api.githubcopilot.com"},
		})
	}))
	defer srv.Close()
	orig := exchangeURL
	exchangeURL = srv.URL
	defer func() { exchangeURL = orig }()

	tok, base, err := exchange(context.Background(), "raw-gh-token")
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok != "copilot-minted" || base != "https://api.githubcopilot.com" {
		t.Fatalf("exchange returned %q / %q", tok, base)
	}
	// Second call is served from the cache — no second network hit.
	if _, _, err := exchange(context.Background(), "raw-gh-token"); err != nil {
		t.Fatalf("cached exchange: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 exchange call (rest cached), got %d", calls)
	}
}

// Resolve wires discovery → exchange → the identity headers the Copilot API
// requires; the integration id is the load-bearing one.
func TestResolveHeaders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("COPILOT_GITHUB_TOKEN", "gho_valid")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ct", "expires_at": 9999999999})
	}))
	defer srv.Close()
	orig := exchangeURL
	exchangeURL = srv.URL
	defer func() { exchangeURL = orig }()

	b, err := Resolve(context.Background())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Headers["Copilot-Integration-Id"] != "vscode-chat" {
		t.Errorf("missing Copilot-Integration-Id: %v", b.Headers)
	}
	if b.Token != "ct" || b.BaseURL != defaultBaseURL {
		t.Errorf("resolve backend = %q / %q", b.Token, b.BaseURL)
	}
}
