package codex

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func makeJWT(t *testing.T, accountID string) string {
	t.Helper()
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	}
	b, _ := json.Marshal(payload)
	seg := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	return seg("{}") + "." + base64.RawURLEncoding.EncodeToString(b) + "." + seg("sig")
}

func TestAccountIDFromJWT(t *testing.T) {
	if got := accountIDFromJWT(makeJWT(t, "acct-123")); got != "acct-123" {
		t.Fatalf("account id = %q, want acct-123", got)
	}
	if got := accountIDFromJWT("not-a-jwt"); got != "" {
		t.Errorf("a non-JWT must yield no account id, got %q", got)
	}
}

// Resolve reads the Codex CLI login and produces the ChatGPT backend with the
// Cloudflare-required originator and the account id from the token.
func TestResolveFromCodexHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	access := makeJWT(t, "acct-xyz")
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": access, "refresh_token": "r"}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Token != access {
		t.Errorf("token not carried through")
	}
	if b.Headers["originator"] != "codex_cli_rs" || b.Headers["ChatGPT-Account-Id"] != "acct-xyz" {
		t.Errorf("codex identity headers wrong: %v", b.Headers)
	}
	if b.BaseURL != baseURL {
		t.Errorf("base = %q", b.BaseURL)
	}
}

func TestResolveNoLogin(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // empty — no auth.json
	if _, err := Resolve(); err == nil {
		t.Fatal("resolve must fail when there's no Codex login")
	}
}
