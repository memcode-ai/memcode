// Package copilot turns a GitHub Copilot subscription into a memcode backend:
// it finds the GitHub token the machine already has (an env var or `gh auth
// token`), exchanges it for a short-lived Copilot API token, and returns the
// endpoint + identity headers the Copilot API requires. No vendor SDK — raw
// HTTP and one subprocess — so it composes into provider selection without a
// dependency cycle (it returns a plain Backend; the provider package adapts it
// to an Endpoint).
//
// This is consent-gated upstream: it activates only when the user explicitly
// selects the copilot source (the first-run wizard, or MEMCODE_CREDENTIAL_SOURCE
// =copilot). It never reads `gh` behind the user's back.
package copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The inference endpoint and the identity Copilot expects. api.githubcopilot.com
// is the default; an enterprise account's exchange response can override it.
const defaultBaseURL = "https://api.githubcopilot.com"

// exchangeURL mints a short-lived Copilot API token from a raw GitHub token.
// A var so tests can point it at a fake server.
var exchangeURL = "https://api.github.com/copilot_internal/v2/token"

// editorVersion is the editor identity the Copilot API keys on; pinned to a
// recent VS Code so requests aren't rejected as too old.
const editorVersion = "vscode/1.104.1"

// copilotEnvVars is the token source ladder: an explicit Copilot var, then the
// conventional gh ones. If ANY is set but invalid we stop rather than shell out
// to `gh` (honor explicit intent, and skip a subprocess on every start).
var copilotEnvVars = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// Backend is the resolved Copilot endpoint: a Bearer token plus the identity
// headers every turn must carry. Consumed by the provider package.
type Backend struct {
	BaseURL string
	Token   string
	Headers map[string]string
}

// inferenceHeaders is the identity a Copilot inference request must present.
// x-initiator=agent marks these as agent turns (memcode is an agent).
func inferenceHeaders() map[string]string {
	return map[string]string{
		"Editor-Version":         editorVersion,
		"Copilot-Integration-Id": "vscode-chat",
		"Openai-Intent":          "conversation-edits",
		"x-initiator":            "agent",
		"User-Agent":             "GitHubCopilotChat/0.26.7",
	}
}

// Resolve finds the GitHub token, exchanges it (using a cached Copilot token
// while fresh), and returns the backend. An error explains the fix (sign in
// with gh, or the token type Copilot won't accept).
func Resolve(ctx context.Context) (Backend, error) {
	raw, err := rawGitHubToken(ctx)
	if err != nil {
		return Backend{}, err
	}
	tok, base, err := exchange(ctx, raw)
	if err != nil {
		return Backend{}, err
	}
	return Backend{BaseURL: base, Token: tok, Headers: inferenceHeaders()}, nil
}

// rawGitHubToken resolves the raw GitHub token from the env ladder, else `gh
// auth token`. A classic PAT (ghp_) is rejected — the Copilot API refuses it.
func rawGitHubToken(ctx context.Context) (string, error) {
	envSeen := false
	for _, v := range copilotEnvVars {
		if val := strings.TrimSpace(os.Getenv(v)); val != "" {
			envSeen = true
			if err := validTokenType(val); err != nil {
				return "", err
			}
			return val, nil
		}
	}
	if envSeen {
		// An env var was set but empty/invalid — do not silently fall back.
		return "", fmt.Errorf("a GitHub token env var is set but unusable")
	}
	tok, err := ghCLIToken(ctx)
	if err != nil {
		return "", err
	}
	if err := validTokenType(tok); err != nil {
		return "", err
	}
	return tok, nil
}

// validTokenType rejects classic PATs (ghp_), which the Copilot token exchange
// 404s; OAuth (gho_), fine-grained (github_pat_), and GitHub-App (ghu_) tokens
// are accepted.
func validTokenType(tok string) error {
	if strings.HasPrefix(tok, "ghp_") {
		return fmt.Errorf("that GitHub token is a classic personal access token, which Copilot won't accept — sign in with `gh auth login` (or use a fine-grained token with Copilot access)")
	}
	return nil
}

// ghCLIToken runs `gh auth token`, probing common install paths. GITHUB_TOKEN
// and GH_TOKEN are stripped from the child env so gh reads its own hosts store
// instead of echoing an env var back.
func ghCLIToken(ctx context.Context) (string, error) {
	bin := ghBinary()
	if bin == "" {
		return "", fmt.Errorf("no GitHub token found — export GH_TOKEN or run `gh auth login`")
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "auth", "token")
	cmd.Env = strippedEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("`gh auth token` failed — run `gh auth login` first")
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("`gh auth token` returned nothing — run `gh auth login`")
	}
	return tok, nil
}

func ghBinary() string {
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, cand := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", filepath.Join(home, ".local/bin/gh")} {
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
	}
	return ""
}

func strippedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// exchange mints (or reuses a cached) Copilot API token for a raw GitHub token
// and returns the token + the inference base URL.
func exchange(ctx context.Context, raw string) (token, baseURL string, err error) {
	fp := fingerprint(raw)
	if ent, ok := readCache(fp); ok && time.Now().Unix() < ent.ExpiresAt-120 {
		return ent.APIToken, ent.BaseURL, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exchangeURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "token "+raw)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("Editor-Version", editorVersion)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("Copilot token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", "", fmt.Errorf("Copilot token exchange returned %s — is Copilot enabled for this account? %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		Endpoints struct {
			API string `json:"api"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("Copilot token exchange decode: %w", err)
	}
	if out.Token == "" {
		return "", "", fmt.Errorf("Copilot token exchange returned an empty token")
	}
	base := strings.TrimRight(out.Endpoints.API, "/")
	if base == "" {
		base = defaultBaseURL
	}
	exp := out.ExpiresAt
	if exp == 0 {
		exp = time.Now().Add(25 * time.Minute).Unix()
	}
	writeCache(fp, cacheEntry{APIToken: out.Token, ExpiresAt: exp, BaseURL: base})
	return out.Token, base, nil
}

func fingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// ── on-disk token cache (0600), keyed by the raw token's fingerprint ──────────

type cacheEntry struct {
	APIToken  string `json:"api_token"`
	ExpiresAt int64  `json:"expires_at"`
	BaseURL   string `json:"base_url"`
}

func cachePath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "memcode", ".copilot_jwt.json")
}

func readCache(fp string) (cacheEntry, bool) {
	p := cachePath()
	if p == "" {
		return cacheEntry{}, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cacheEntry{}, false
	}
	var m map[string]cacheEntry
	if json.Unmarshal(b, &m) != nil {
		return cacheEntry{}, false
	}
	ent, ok := m[fp]
	return ent, ok
}

// writeCache stores the entry, pruning expired ones. Best-effort: a cache write
// failure just means the next call re-exchanges. Atomic + 0600, and the token
// never lands in a world-readable file.
func writeCache(fp string, ent cacheEntry) {
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	m := map[string]cacheEntry{}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	now := time.Now().Unix()
	for k, e := range m {
		if e.ExpiresAt <= now {
			delete(m, k)
		}
	}
	m[fp] = ent
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// Available reports whether a GitHub token the exchange could use is present —
// an env var or a gh install — without exchanging. For the wizard's menu.
func Available() bool {
	for _, v := range copilotEnvVars {
		if strings.TrimSpace(os.Getenv(v)) != "" {
			return true
		}
	}
	return ghBinary() != ""
}
