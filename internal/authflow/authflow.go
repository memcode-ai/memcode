// Package authflow is the browser login flow shared by `memcode login` (cobra)
// and the TUI's /login slash command. A local HTTP server on 127.0.0.1:19090
// receives the token from the web app's /api/cli/auth callback redirect; the
// caller persists it via WriteGlobalEnvToken so the existing LoadDotEnv →
// provider path picks it up.
package authflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/provider"
)

// DefaultGatewayURL is the production memcode gateway. The web app's
// /api/cli/auth may override this by returning api_url in the callback, but
// this default lets login work even when the callback doesn't include it.
const DefaultGatewayURL = provider.DefaultAPIURL

// DefaultWebAppURL is where the browser opens to authenticate.
const DefaultWebAppURL = "https://memcode.ai"

// Result is a successful login: the minted org key and the gateway to use it
// against.
type Result struct {
	Token      string
	GatewayURL string
}

// loginResult is the outcome of the browser callback.
type loginResult struct {
	token  string
	apiURL string
	err    string
}

// makeCallbackHandler returns an http.HandlerFunc that validates the state
// parameter and sends the result (token or error) down resultCh.
func makeCallbackHandler(state string, resultCh chan<- loginResult) http.HandlerFunc {
	// deliver is non-blocking: resultCh has capacity 1 and only the FIRST
	// outcome matters. A duplicate success callback (browser re-GET/prefetch)
	// must not block its handler goroutine forever on a full channel.
	deliver := func(res loginResult) {
		select {
		case resultCh <- res:
		default:
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			// A stray local request (another process probing the port, a
			// prefetch of a stale URL) must NOT abort the pending login —
			// only the request carrying OUR state speaks for this flow.
			http.NotFound(w, r)
			return
		}
		if errMsg := q.Get("error"); errMsg != "" {
			deliver(loginResult{err: errMsg})
			writeCallbackPage(w, false, errMsg)
			return
		}
		token := q.Get("token")
		if token == "" {
			deliver(loginResult{err: "no token received"})
			writeCallbackPage(w, false, "No token received.")
			return
		}
		// The web app may include api_url to tell us the gateway endpoint.
		apiURL := q.Get("api_url")
		deliver(loginResult{token: token, apiURL: apiURL})
		writeCallbackPage(w, true, "")
	}
}

// Run executes the browser login flow: starts the local callback server, opens
// the browser, and waits for the redirect. status (nil-safe) receives progress
// lines for display. Cancel ctx to abort (the TUI's Esc); an internal 2-minute
// timeout applies regardless. Run does NOT persist anything — callers decide
// (WriteGlobalEnvToken).
func Run(ctx context.Context, status func(string)) (Result, error) {
	say := func(s string) {
		if status != nil {
			status(s)
		}
	}

	// Generate CSRF state.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return Result{}, fmt.Errorf("failed to generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	// Start local callback server. 19090 is the stable first choice, but the
	// callback URL carries the port dynamically, so anything else on 19090
	// (common dev-port territory) just moves us to an OS-assigned free port —
	// a first-time user must never be unable to sign in over a busy port.
	listener, err := net.Listen("tcp", "127.0.0.1:19090")
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return Result{}, fmt.Errorf("failed to start local callback server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan loginResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", makeCallbackHandler(state, resultCh))

	server := &http.Server{Handler: mux}
	go server.Serve(listener)

	webAppURL := envOr("MEMCODE_WEB_APP_URL", DefaultWebAppURL)
	authURL := fmt.Sprintf("%s/api/cli/auth?port=%d&state=%s", webAppURL, port, state)

	say("Opening your browser to authenticate...")
	say("If it doesn't open, visit:\n    " + authURL)

	// NEVER from a test binary: a signed-in browser would complete the real
	// flow against prod and mint a live key (it happened — the key was revoked
	// and every `go test ./...` popped browser tabs until this guard).
	if testing.Testing() {
		// no-op: tests drive the callback directly or cancel the context
	} else if err := openBrowser(authURL); err != nil {
		say(fmt.Sprintf("(could not open browser automatically: %v)", err))
	}

	say("Waiting for authentication…")

	select {
	case res := <-resultCh:
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		server.Shutdown(sctx)
		cancel()

		if res.err != "" {
			return Result{}, fmt.Errorf("authentication failed: %s", res.err)
		}

		// Determine the gateway URL: callback override > env > default.
		gatewayURL := res.apiURL
		if gatewayURL == "" {
			gatewayURL = envOr(provider.EnvAPIURL, DefaultGatewayURL)
		}
		return Result{Token: res.token, GatewayURL: gatewayURL}, nil

	case <-ctx.Done():
		server.Close()
		return Result{}, fmt.Errorf("login canceled")

	case <-time.After(2 * time.Minute):
		server.Close()
		return Result{}, fmt.Errorf("timed out waiting for authentication (2 min). Try again.")
	}
}

// WriteGlobalEnvToken writes MEMCODE_API_TOKEN and MEMCODE_API_URL into the
// global env file (~/.config/memcode/.env), creating it if needed and
// replacing any existing values for those keys (no-override is for LoadDotEnv;
// login is the explicit user action that SHOULD overwrite).
func WriteGlobalEnvToken(token, apiURL string) error {
	path := provider.GlobalEnvPath()
	if path == "" {
		return fmt.Errorf("cannot determine global config path (no home directory)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	// Read existing lines, drop any old API token / URL lines.
	var lines []string
	if existing, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(existing), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				lines = append(lines, line)
				continue
			}
			stripped := strings.TrimPrefix(trimmed, "export ")
			key, _, ok := strings.Cut(stripped, "=")
			if ok && (strings.TrimSpace(key) == provider.EnvAPIToken || strings.TrimSpace(key) == provider.EnvAPIURL) {
				continue // drop old value
			}
			lines = append(lines, line)
		}
	}

	// Append the fresh values.
	lines = append(lines, fmt.Sprintf("%s=%s", provider.EnvAPIToken, token))
	lines = append(lines, fmt.Sprintf("%s=%s", provider.EnvAPIURL, apiURL))

	content := strings.Join(lines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0600)
}

// StripGlobalEnvToken removes the token/url lines from the global env file
// (logout). Returns whether anything was removed; a missing file is a no-op.
// The token stays valid server-side until revoked from the web app
// (a /api/cli/revoke endpoint is a follow-up).
func StripGlobalEnvToken() (bool, error) {
	path := provider.GlobalEnvPath()
	if path == "" {
		return false, fmt.Errorf("cannot determine global config path (no home directory)")
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read config: %w", err)
	}
	var lines []string
	removed := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			lines = append(lines, line)
			continue
		}
		stripped := strings.TrimPrefix(trimmed, "export ")
		key, _, ok := strings.Cut(stripped, "=")
		if ok && (strings.TrimSpace(key) == provider.EnvAPIToken || strings.TrimSpace(key) == provider.EnvAPIURL) {
			removed = true
			continue
		}
		lines = append(lines, line)
	}
	if !removed {
		return false, nil
	}
	content := strings.Join(lines, "\n")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return false, fmt.Errorf("failed to write config: %w", err)
	}
	return true, nil
}

func writeCallbackPage(w http.ResponseWriter, ok bool, errMsg string) {
	w.Header().Set("Content-Type", "text/html")
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
	}
	body := `<html><body style="font-family:system-ui;display:flex;justify-content:center;align-items:center;height:100vh;margin:0;background:#0a0a0a;color:#fafafa"><div style="text-align:center">`
	if ok {
		body += `<h2>Authenticated</h2><p style="color:#888">You can close this tab and return to your terminal.</p>`
	} else {
		body += fmt.Sprintf(`<h2>Authentication failed</h2><p style="color:#f87171">%s</p><p style="color:#888">Close this tab and try again.</p>`, errMsg)
	}
	body += `</div></body></html>`
	io.WriteString(w, body)
}

// envOr returns os.Getenv(key) or fallback if empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// openBrowser opens url in the user's default browser. Best-effort.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default: // linux, *bsd
		for _, cmd := range []string{"xdg-open", "wslview", "gio"} {
			if _, err := exec.LookPath(cmd); err == nil {
				return exec.Command(cmd, url).Start()
			}
		}
		return fmt.Errorf("no browser-opening command found")
	}
}
