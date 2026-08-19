// Package codex turns a ChatGPT (Codex) subscription into a memcode backend by
// reusing the login the Codex CLI already stored. It reads ~/.codex/auth.json
// (or $CODEX_HOME), refreshes the access token when it's expiring, pulls the
// account id out of the access-token JWT, and returns the ChatGPT/Codex endpoint
// plus the identity headers Cloudflare and the backend require. No vendor SDK —
// file read + OAuth refresh + JWT decode — so it returns a plain Backend the
// provider package adapts to an endpoint.
//
// Consent-gated upstream: it activates only when the user explicitly selects the
// codex source. It reuses the Codex CLI's file and, on a refresh, writes the
// rotated tokens back there so both tools stay in sync on the single-use
// refresh token.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// resolveMu serializes Resolve's read→refresh→write-back: Codex refresh tokens are single-use
// and rotate, so two concurrent Resolves could both spend the same refresh token and clobber
// the rotated replacement on write-back. One mutex per credential file.
var resolveMu sync.Mutex

// Backend is the resolved Codex endpoint: the ChatGPT backend base, the access
// token (Bearer), and the identity headers every turn must carry.
type Backend struct {
	BaseURL string
	Token   string
	Headers map[string]string
}

// baseURL is the ChatGPT Codex backend; the Responses adapter targets
// {base}/responses.
const baseURL = "https://chatgpt.com/backend-api/codex"

// codexClientID is the OAuth client the Codex CLI uses; required to refresh.
const codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// codexTokenURL is the OpenAI OAuth token endpoint. A var for tests.
var codexTokenURL = "https://auth.openai.com/oauth/token"

// errCodexQuota signals a 429 from the token endpoint: the refresh token is
// still valid (a quota cap, not an auth failure), so the caller keeps the
// existing access token rather than forcing a re-login.
var errCodexQuota = errors.New("codex token endpoint rate-limited")

// codexCreds is the reconciled Codex login.
type codexCreds struct {
	Access  string
	Refresh string
	IDToken string
}

// Resolve reads the Codex CLI login, refreshes it if the access token is
// expiring, and shapes it into a backend. An error explains the fix
// (install/sign in with the Codex CLI).
func Resolve() (Backend, error) {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	c, err := readCreds()
	if err != nil {
		return Backend{}, err
	}
	access := c.Access
	if expiringSoon(access) {
		if c.Refresh == "" {
			return Backend{}, fmt.Errorf("the Codex login is expired and can't be refreshed — sign in again with `codex login`")
		}
		refreshed, rerr := refresh(c.Refresh)
		switch {
		case rerr == nil:
			writeBack(refreshed)
			access = refreshed.Access
		case errors.Is(rerr, errCodexQuota):
			// Quota cap, not an auth failure: the current token is still valid,
			// so serve it as-is.
		default:
			return Backend{}, rerr
		}
	}
	account := accountIDFromJWT(access)
	if account == "" {
		return Backend{}, fmt.Errorf("the Codex login has no ChatGPT account id — sign in again with the Codex CLI")
	}
	return Backend{
		BaseURL: baseURL,
		Token:   access,
		Headers: map[string]string{
			// Cloudflare only admits a whitelisted originator; the account id
			// selects the subscription. codex_cli_rs is the accepted originator.
			"User-Agent":         "codex_cli_rs/0.0.0 (memcode)",
			"originator":         "codex_cli_rs",
			"ChatGPT-Account-Id": account,
		},
	}, nil
}

// codexAuth is the subset of ~/.codex/auth.json we read.
type codexAuth struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	} `json:"tokens"`
}

func authPath() string {
	if h := strings.TrimSpace(os.Getenv("CODEX_HOME")); h != "" {
		return filepath.Join(h, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

func readCreds() (codexCreds, error) {
	p := authPath()
	if p == "" {
		return codexCreds{}, fmt.Errorf("no Codex login found — install the Codex CLI and run `codex login`")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return codexCreds{}, fmt.Errorf("no Codex login at %s — run `codex login`", p)
	}
	// utf-8-sig tolerance: strip a leading BOM the CLI may write.
	b = trimBOM(b)
	var a codexAuth
	if err := json.Unmarshal(b, &a); err != nil {
		return codexCreds{}, fmt.Errorf("codex auth.json is unreadable: %w", err)
	}
	if a.Tokens.AccessToken == "" {
		return codexCreds{}, fmt.Errorf("codex auth.json has no access token — run `codex login`")
	}
	return codexCreds{Access: a.Tokens.AccessToken, Refresh: a.Tokens.RefreshToken, IDToken: a.Tokens.IDToken}, nil
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// expiringSoon reports whether the access token's JWT expiry is within 60s.
// An unparseable expiry returns false so a token in an unexpected shape is used
// as-is rather than forced through a refresh that may not be needed.
func expiringSoon(access string) bool {
	exp, ok := jwtExp(access)
	if !ok {
		return false
	}
	return time.Now().Unix() >= exp-60
}

// jwtExp reads the numeric "exp" (epoch seconds) claim from a JWT.
func jwtExp(tok string) (int64, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp == 0 {
		return 0, false
	}
	return claims.Exp, true
}

// refresh exchanges the refresh token for a new access token at the OpenAI OAuth
// endpoint (form-encoded, per the Codex CLI). A 429 is a usage-quota cap, not an
// auth failure, so it maps to errCodexQuota and the caller keeps the old token.
func refresh(refreshToken string) (codexCreds, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {codexClientID},
	}
	req, err := http.NewRequest(http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return codexCreds{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return codexCreds{}, fmt.Errorf("codex token refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return codexCreds{}, errCodexQuota
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return codexCreds{}, fmt.Errorf("codex token refresh returned %s — sign in again with `codex login`: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return codexCreds{}, fmt.Errorf("codex token refresh decode: %w", err)
	}
	if out.AccessToken == "" {
		return codexCreds{}, fmt.Errorf("codex token refresh returned an empty token")
	}
	nc := codexCreds{Access: out.AccessToken, Refresh: out.RefreshToken, IDToken: out.IDToken}
	// Refresh tokens rotate and are single-use; keep the old one only if the
	// response omitted a replacement.
	if nc.Refresh == "" {
		nc.Refresh = refreshToken
	}
	return nc, nil
}

// writeBack persists refreshed tokens into the Codex CLI's own auth.json so the
// rotated, single-use refresh token stays in sync for both tools. Best-effort,
// atomic, 0600. Unrelated fields in the file are preserved.
func writeBack(c codexCreds) {
	p := authPath()
	if p == "" {
		return
	}
	m := map[string]any{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(trimBOM(b), &m)
	}
	tokens, _ := m["tokens"].(map[string]any)
	if tokens == nil {
		tokens = map[string]any{}
	}
	tokens["access_token"] = c.Access
	tokens["refresh_token"] = c.Refresh
	if c.IDToken != "" {
		tokens["id_token"] = c.IDToken
	}
	m["tokens"] = tokens
	m["last_refresh"] = time.Now().UTC().Format(time.RFC3339)

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

// accountIDFromJWT extracts the ChatGPT account id from the access-token JWT's
// claims — payload["https://api.openai.com/auth"]["chatgpt_account_id"]. Returns
// "" if the token isn't a JWT or the claim is absent.
func accountIDFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// Available reports whether a Codex CLI login file is present, for the wizard's
// menu (no parse, no network).
func Available() bool {
	p := authPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
