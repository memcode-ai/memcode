package codex

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func makeJWT(t *testing.T, accountID string) string {
	t.Helper()
	return makeJWTExp(t, accountID, 0)
}

// makeJWTExp builds a token with the account-id claim and, when exp != 0, an
// expiry (epoch seconds).
func makeJWTExp(t *testing.T, accountID string, exp int64) string {
	t.Helper()
	payload := map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID},
	}
	if exp != 0 {
		payload["exp"] = exp
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

// An expired access token is refreshed against the token endpoint, and the
// rotated tokens are written back to the Codex CLI's auth.json.
func TestResolveRefreshesAndWritesBack(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)

	expired := makeJWTExp(t, "acct-old", time.Now().Add(-time.Hour).Unix())
	fresh := makeJWTExp(t, "acct-new", time.Now().Add(time.Hour).Unix())
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": expired, "refresh_token": "r-old"}})
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "r-old" || r.Form.Get("client_id") != codexClientID {
			t.Errorf("refresh form wrong: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fresh, "refresh_token": "r-new", "id_token": "id-new",
		})
	}))
	defer srv.Close()
	orig := codexTokenURL
	codexTokenURL = srv.URL
	defer func() { codexTokenURL = orig }()

	b, err := Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b.Token != fresh {
		t.Fatalf("resolve did not use the refreshed token")
	}
	if b.Headers["ChatGPT-Account-Id"] != "acct-new" {
		t.Errorf("account id not from the fresh token: %v", b.Headers)
	}

	// The rotated tokens were written back to auth.json.
	back, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	var f codexAuth
	_ = json.Unmarshal(back, &f)
	if f.Tokens.AccessToken != fresh || f.Tokens.RefreshToken != "r-new" || f.Tokens.IDToken != "id-new" {
		t.Errorf("tokens not written back: %+v", f.Tokens)
	}
}

// An expired token with no refresh token can't be recovered and must error.
func TestResolveExpiredNoRefresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	expired := makeJWTExp(t, "acct-old", time.Now().Add(-time.Hour).Unix())
	body, _ := json.Marshal(map[string]any{"tokens": map[string]string{"access_token": expired}})
	_ = os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600)
	if _, err := Resolve(); err == nil {
		t.Fatal("an expired token with no refresh token must error")
	}
}
