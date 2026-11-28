// Package claudesub turns a Claude Pro/Max subscription into a memcode backend
// by reusing the login the Claude Code CLI already stored. It reads the OAuth
// token from ~/.claude/.credentials.json (and, on macOS, the Keychain),
// reconciles the two, refreshes an expired token, and returns the access token.
// A running session holds a TokenSource rather than a fixed token, so a token
// that expires mid-session, or one replaced by a Claude Code re-auth, is picked
// up on the next request (and a 401 retries once) without restarting memcode.
// The token is then used through the Anthropic adapter's Claude Code
// compatibility mode (internal/providers/anthropic/oauth.go), where the actual
// impersonation lives — this package only resolves the credential.
//
// Consent-gated upstream: it activates only when the user explicitly selects the
// claude source. A refreshed token is written back to wherever the login came
// from: Claude Code's file (atomically, 0600), or on macOS its Keychain item.
// Refresh tokens are single-use, so writing anywhere else would sign Claude
// Code out.
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
	"sync"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// resolveMu serializes Resolve's read→refresh→write: refresh tokens are single-use and
// rotate, so two concurrent Resolves could both spend the same refresh token (the second
// exchange fails) and clobber the rotated one on write-back. One mutex per credential file.
var resolveMu sync.Mutex

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
	fromKeychain bool // read from the macOS Keychain (Claude Code ≥2.1.114), not the file
}

// Resolve returns a usable Claude access token, refreshing it if expired. The
// returned token is an OAuth token (cc-*/eyJ…) that turns on the adapter's
// compatibility mode.
func Resolve() (string, error) {
	c, err := resolve("")
	return c.AccessToken, err
}

// resolve reads the current login (file + Keychain, freshest wins) and returns
// it, refreshing when it is expired OR when it is exactly the token the API
// just rejected. A token that differs from rejected and is unexpired is used
// as-is: that is the "user re-authed Claude Code" case, picked up for free.
func resolve(rejected string) (creds, error) {
	resolveMu.Lock()
	defer resolveMu.Unlock()
	c, err := readCreds()
	if err != nil {
		return creds{}, err
	}
	if valid(c.ExpiresAt) && (rejected == "" || c.AccessToken != rejected) {
		return c, nil
	}
	if c.RefreshToken == "" {
		return creds{}, fmt.Errorf("the Claude login is expired and can't be refreshed — sign in again with Claude Code")
	}
	refreshed, err := refresh(c)
	if err != nil {
		return creds{}, err
	}
	if err := writeBack(c, refreshed); err != nil {
		// The old refresh token is spent; the new one exists only in memory. Use
		// it for this session and say plainly what the user may need to do.
		return refreshed, fmt.Errorf("refreshed the Claude login but couldn't save it (%v) — if Claude Code asks you to sign in again, that is why", err)
	}
	return refreshed, nil
}

// TokenSource hands a long-lived session the CURRENT Claude access token,
// instead of the one captured at startup. Claude Code tokens live for hours;
// a session that outlives one (or a user who re-auths Claude Code mid-session)
// must self-heal without restarting memcode. Token is cheap on the hot path
// (an in-memory copy until it nears expiry); Rejected is the 401 path.
type TokenSource struct {
	mu  sync.Mutex
	tok string
	exp int64 // epoch ms; 0 = no expiry known
}

// NewTokenSource seeds a source with an already-resolved token so the first
// request needs no extra credential read.
func NewTokenSource(tok string) *TokenSource { return &TokenSource{tok: tok, exp: 0} }

// Token returns a usable access token, re-resolving (re-read + refresh) when
// the cached one is near expiry. exp==0 on a seeded source means "unknown",
// so the first expiry check after seeding consults the credential store once.
func (s *TokenSource) Token() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && s.exp != 0 && valid(s.exp) {
		return s.tok, nil
	}
	c, err := resolve("")
	if err != nil {
		if s.tok != "" && s.exp == 0 {
			return s.tok, nil // credential store unreadable: keep serving the seed; a 401 heals via Rejected
		}
		return "", err
	}
	s.tok, s.exp = c.AccessToken, c.ExpiresAt
	if s.exp == 0 {
		s.exp = time.Now().Add(5 * time.Minute).UnixMilli() // managed key: re-check occasionally
	}
	return s.tok, nil
}

// Rejected is called when the API refused tok (401). It re-reads the login —
// picking up a fresh Claude Code sign-in — or refreshes it, and returns the
// replacement. An error means there is genuinely nothing better to try.
func (s *TokenSource) Rejected(tok string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok != "" && s.tok != tok && s.exp != 0 && valid(s.exp) {
		return s.tok, nil // a concurrent request already healed it
	}
	c, err := resolve(tok)
	if err != nil {
		return "", err
	}
	if c.AccessToken == tok {
		return "", fmt.Errorf("the Claude login was rejected — sign in again with Claude Code")
	}
	s.tok, s.exp = c.AccessToken, c.ExpiresAt
	if s.exp == 0 {
		s.exp = time.Now().Add(5 * time.Minute).UnixMilli()
	}
	return s.tok, nil
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
			kc.fromKeychain = true
			return kc, nil
		}
		return file, nil
	case fileOK:
		return file, nil
	case kcOK:
		kc.fromKeychain = true
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
		return creds{}, fmt.Errorf("claude token refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return creds{}, fmt.Errorf("claude token refresh returned %s — sign in again with Claude Code: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return creds{}, fmt.Errorf("claude token refresh decode: %w", err)
	}
	if out.AccessToken == "" {
		return creds{}, fmt.Errorf("claude token refresh returned an empty token")
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

// writeBack saves a refreshed login where it was read from, so the rotated,
// single-use refresh token stays valid for Claude Code too. A Keychain login
// is never copied into a plaintext file; the file is updated only when the
// login came from it or it already exists.
func writeBack(old, c creds) error {
	if old.fromKeychain {
		if err := writeBackKeychain(c); err != nil {
			return err
		}
		if p := credFilePath(); p != "" {
			if _, err := os.Stat(p); err == nil {
				return writeBackFile(c)
			}
		}
		return nil
	}
	return writeBackFile(c)
}

// oauthJSON merges the refreshed fields into an existing credential document,
// leaving unrelated fields (other logins, MCP tokens) untouched.
func oauthJSON(existing []byte, c creds) ([]byte, error) {
	m := map[string]any{}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &m)
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = []string{"user:inference", "user:profile"}
	}
	o, _ := m["claudeAiOauth"].(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	o["accessToken"], o["refreshToken"], o["expiresAt"], o["scopes"] = c.AccessToken, c.RefreshToken, c.ExpiresAt, scopes
	m["claudeAiOauth"] = o
	return json.Marshal(m)
}

// writeBackFile persists a refreshed token into Claude Code's own file,
// atomically at 0600.
func writeBackFile(c creds) error {
	p := credFilePath()
	if p == "" {
		return fmt.Errorf("no home directory")
	}
	existing, _ := os.ReadFile(p)
	b, err := oauthJSON(existing, c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return atomicfile.WriteFile(p, b, 0o600)
}

const keychainService = "Claude Code-credentials"

// writeBackKeychain updates Claude Code's Keychain item in place. The secret
// goes to `security` on stdin (hex-encoded, in its interactive mode), never on
// the command line where other processes could read it.
func writeBackKeychain(c creds) error {
	if !keychainEnabled || runtime.GOOS != "darwin" {
		return nil
	}
	existing, _ := exec.Command("security", "find-generic-password", "-s", keychainService, "-w").Output()
	b, err := oauthJSON(bytes.TrimSpace(existing), c)
	if err != nil {
		return err
	}
	attrs, err := exec.Command("security", "find-generic-password", "-s", keychainService).Output()
	if err != nil {
		return fmt.Errorf("keychain item not found: %w", err)
	}
	acct := keychainAccount(string(attrs))
	if acct == "" || strings.ContainsAny(acct, "\"\n\\") {
		return fmt.Errorf("keychain item has no usable account name")
	}
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -a \"%s\" -s \"%s\" -X %x\n", acct, keychainService, b))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("keychain update failed: %v %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// keychainAccount pulls the account attribute out of `security
// find-generic-password` output: a line like `    "acct"<blob>="name"`.
func keychainAccount(attrs string) string {
	for _, ln := range strings.Split(attrs, "\n") {
		ln = strings.TrimSpace(ln)
		if rest, ok := strings.CutPrefix(ln, `"acct"<blob>="`); ok {
			return strings.TrimSuffix(rest, `"`)
		}
	}
	return ""
}

// Available reports whether a Claude Code login is present (file, or the macOS
// Keychain), for the wizard's menu.
func Available() bool {
	if p := credFilePath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if _, ok := readKeychainCreds(); ok {
		return true
	}
	return false
}
