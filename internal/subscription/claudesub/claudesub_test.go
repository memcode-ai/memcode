package claudesub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func init() { keychainEnabled = false } // deterministic: file source only in tests

func writeCredFile(t *testing.T, home string, c map[string]any) {
	t.Helper()
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"claudeAiOauth": c})
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A live token is returned as-is; no refresh, no network.
func TestResolveFreshToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, home, map[string]any{
		"accessToken":  "cc-fresh",
		"refreshToken": "r1",
		"expiresAt":    time.Now().Add(time.Hour).UnixMilli(),
	})
	tok, err := Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "cc-fresh" {
		t.Fatalf("token = %q, want cc-fresh", tok)
	}
}

// An expired token is refreshed against the token endpoint, and the new token
// is written back to Claude Code's file.
func TestResolveRefreshesAndWritesBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeCredFile(t, home, map[string]any{
		"accessToken":  "cc-old",
		"refreshToken": "r-old",
		"expiresAt":    time.Now().Add(-time.Hour).UnixMilli(), // expired
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "axios/1.7.9" {
			t.Errorf("refresh UA = %q, want axios/1.7.9 (claude-code/* is 429'd on the token endpoint)", ua)
		}
		_ = r.ParseForm()
		if r.Form.Get("refresh_token") != "r-old" || r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("refresh form wrong: %v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "cc-new", "refresh_token": "r-new", "expires_in": 3600,
		})
	}))
	defer srv.Close()
	orig := refreshURL
	refreshURL = srv.URL
	defer func() { refreshURL = orig }()

	tok, err := Resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if tok != "cc-new" {
		t.Fatalf("token = %q, want cc-new", tok)
	}
	// Written back so both tools stay in sync on the single-use refresh token.
	back, _ := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	var f claudeCredFile
	_ = json.Unmarshal(back, &f)
	if f.ClaudeAiOauth.AccessToken != "cc-new" || f.ClaudeAiOauth.RefreshToken != "r-new" {
		t.Errorf("token not written back: %+v", f.ClaudeAiOauth)
	}
}

func TestResolveNoLogin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Resolve(); err == nil {
		t.Fatal("resolve must fail with no Claude login")
	}
}
