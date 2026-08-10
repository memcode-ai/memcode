// Package claudesub turns a Claude Pro/Max subscription into a memcode backend
// by reusing the login the Claude Code CLI already stored. It reads the OAuth
// token from ~/.claude/.credentials.json (and, on macOS, the Keychain),
// reconciles the two, refreshes an expired token, and returns the access token.
// The token is then used through the Anthropic adapter's Claude Code
// compatibility mode (internal/providers/anthropic/oauth.go), where the actual
// impersonation lives — this package only resolves the credential.
//
// Consent-gated upstream: it activates only when the user explicitly selects the
// claude source. It reuses Claude Code's file; refreshed tokens are written back
// there (never the Keychain), atomically at 0600.
package claudesub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// oauthClientID is the client id the Claude Code OAuth flow uses; required to
// refresh the token.
const oauthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

// refreshURL is the token endpoint (with a fallback). A var for tests.
var refreshURL = "https://platform.claude.com/v1/oauth/token"

// creds is the reconciled credential.
type creds struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // epoch milliseconds
	Scopes       []string
}

// Resolve returns a usable Claude access token, refreshing it if expired. The
// returned token is an OAuth token (cc-*/eyJ…) that turns on the adapter's
// compatibility mode.
func Resolve() (string, error) {
	c, err := readCreds()
	if err != nil {
		return "", err
	}
	if valid(c.ExpiresAt) {
		return c.AccessToken, nil
	}
	if c.RefreshToken == "" {
		return "", fmt.Errorf("the Claude login is expired and can't be refreshed — sign in again with Claude Code")
	}
	refreshed, err := refresh(c)
	if err != nil {
		return "", err
	}
	writeBackFile(refreshed)
	return refreshed.AccessToken, nil
}

// valid reports whether an epoch-ms expiry is at least 60s in the future. A zero
// expiry is treated as valid iff there's a token (a managed key with no expiry).
func valid(expiresAtMs int64) bool {
	if expiresAtMs == 0 {
		return true
	}
	return time.Now().UnixMilli() < expiresAtMs-60_000
}

// readCreds reads the file and (on macOS) the Keychain, returning whichever is
// present, or the one with the later expiry when both are — so a refresh uses
// the freshest refresh token.
func readCreds() (creds, error) {
	file, fileOK := readFileCreds()
	kc, kcOK := readKeychainCreds()
	switch {
	case fileOK && kcOK:
		if kc.ExpiresAt > file.ExpiresAt {
			return kc, nil
		}
		return file, nil
	case fileOK:
		return file, nil
	case kcOK:
		return kc, nil
	}
	return creds{}, fmt.Errorf("no Claude login found — sign in with Claude Code first")
}

// claudeCredFile is the on-disk shape: the OAuth fields nest under claudeAiOauth.
type claudeCredFile struct {
	ClaudeAiOauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"`
		Scopes       []string `json:"scopes"`
	} `json:"claudeAiOauth"`
}

func credFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func parseCreds(b []byte) (creds, bool) {
	var f claudeCredFile
	if json.Unmarshal(b, &f) != nil || f.ClaudeAiOauth.AccessToken == "" {
		return creds{}, false
	}
	o := f.ClaudeAiOauth
	return creds{AccessToken: o.AccessToken, RefreshToken: o.RefreshToken, ExpiresAt: o.ExpiresAt, Scopes: o.Scopes}, true
}

func readFileCreds() (creds, bool) {
	p := credFilePath()
	if p == "" {
		return creds{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return creds{}, false
	}
	return parseCreds(b)
}

// readKeychainCreds reads the "Claude Code-credentials" generic password on
// macOS (where Claude Code ≥2.1.114 stores it). No-op elsewhere.
// keychainEnabled gates the macOS Keychain read; tests disable it so a real
// Claude Code login on the dev machine can't make them non-deterministic.
var keychainEnabled = true

func readKeychainCreds() (creds, bool) {
	if !keychainEnabled || runtime.GOOS != "darwin" {
		return creds{}, false
	}
	cmd := exec.Command("security", "find-generic-password", "-s", "Claude Code-credentials", "-w")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		return creds{}, false
	}
	return parseCreds(bytes.TrimSpace(out))
}

// refresh exchanges the refresh token for a new access token. UA axios/1.7.9 —
// Anthropic 429s a claude-code/* UA on the token endpoint (that UA is for the
// inference path only).
func refresh(c creds) (creds, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.RefreshToken},
		"client_id":     {oauthClientID},
	}
	req, err := http.NewRequest(http.MethodPost, refreshURL, strings.NewReader(form.Encode()))
	if err != nil {
		return creds{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "axios/1.7.9")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return creds{}, fmt.Errorf("Claude token refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return creds{}, fmt.Errorf("Claude token refresh returned %s — sign in again with Claude Code: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return creds{}, fmt.Errorf("Claude token refresh decode: %w", err)
	}
	if out.AccessToken == "" {
		return creds{}, fmt.Errorf("Claude token refresh returned an empty token")
	}
	nc := creds{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken, Scopes: c.Scopes}
	if nc.RefreshToken == "" {
		nc.RefreshToken = c.RefreshToken
	}
	expIn := out.ExpiresIn
	if expIn == 0 {
		expIn = 3600
	}
	nc.ExpiresAt = time.Now().UnixMilli() + expIn*1000
	return nc, nil
}

// writeBackFile persists a refreshed token into Claude Code's own file so the
// single-use refresh token stays in sync for both tools. Best-effort, atomic,
// 0600. The Keychain is never written.
func writeBackFile(c creds) {
	p := credFilePath()
	if p == "" {
		return
	}
	// Merge into the existing JSON so unrelated fields are preserved.
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = []string{"user:inference", "user:profile"}
	}
	m["claudeAiOauth"] = map[string]any{
		"accessToken":  c.AccessToken,
		"refreshToken": c.RefreshToken,
		"expiresAt":    c.ExpiresAt,
		"scopes":       scopes,
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
