package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// OAuth for remote servers, the Claude-Code way: when a server answers 401/403, run the
// interactive authorization-code flow — register dynamically, open the user's browser to the
// authorization URL, and catch the redirect on a loopback listener. The SDK drives the flow
// (auth.AuthorizationCodeHandler); this file supplies the two pieces it can't: a redirect sink
// and a way to open a browser. Only attached in interactive sessions (a headless run has no one
// to complete the flow).

// oauthHandler builds an OAuth handler bound to a freshly-allocated loopback redirect. The
// returned closer shuts down the callback server and releases the listener (call it when the
// connection is torn down). httpClient is used for the SDK's metadata/registration/token
// requests.
//
// The callback server is started ONCE and lives for the whole connection, because the flow can
// run more than once per connection (token expiry → re-authorization): the redirect URI is
// registered with the authorization server at dynamic-registration time, so its port must stay
// stable, and a per-flow Shutdown would close the shared listener and leave every later flow
// blocking on a dead socket (the old bug).
func oauthHandler(httpClient *http.Client) (auth.OAuthHandler, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, func() {}, err
	}
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	sink := &codeSink{}
	srv := &http.Server{Handler: sink}
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			// Surface the failure to any current or future flow instead of hanging it.
			sink.fail(fmt.Errorf("oauth callback server: %w", serr))
		}
	}()
	closer := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx) // closes ln too
	}

	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs: []string{redirect},
				ClientName:   "memcode",
			},
		},
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: sink.fetch,
		Client:                   httpClient,
	})
	if err != nil {
		closer()
		return nil, func() {}, err
	}
	return h, closer, nil
}

// codeSink is the loopback redirect target, reusable across authorization flows on one
// connection: each fetch installs a fresh result channel, opens the browser, and waits for
// the redirect (or a server failure) to land.
type codeSink struct {
	mu      sync.Mutex
	current chan sinkResult // the active flow's result channel; nil when no flow is waiting
	err     error           // a fatal server error, failing every current and future flow
}

type sinkResult struct {
	result *auth.AuthorizationResult
	err    error
}

// deliver hands the outcome to the active flow, if any (non-blocking: capacity 1, first
// outcome wins — a duplicate redirect or stray hit must never wedge a handler goroutine).
func (s *codeSink) deliver(r sinkResult) bool {
	s.mu.Lock()
	ch := s.current
	s.mu.Unlock()
	if ch == nil {
		return false
	}
	select {
	case ch <- r:
		return true
	default:
		return false
	}
}

// fail marks the sink dead (the server stopped serving) and unblocks any waiting flow.
func (s *codeSink) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.deliver(sinkResult{nil, err})
}

// ServeHTTP captures the authorization redirect for the active flow.
func (s *codeSink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
		s.deliver(sinkResult{nil, fmt.Errorf("authorization error: %s", e)})
		return
	}
	code := q.Get("code")
	if code == "" {
		http.NotFound(w, r) // favicon and other stray hits
		return
	}
	if !s.deliver(sinkResult{&auth.AuthorizationResult{Code: code, State: q.Get("state")}, nil}) {
		http.Error(w, "no authorization flow is waiting", http.StatusConflict)
		return
	}
	_, _ = w.Write([]byte("<html><body>Authorized. You can close this tab and return to memcode.</body></html>"))
}

// fetch is the auth.AuthorizationCodeFetcher: it opens the authorization URL in the user's
// browser and waits for the loopback redirect. Reusable — every invocation installs a fresh
// result channel on the long-lived callback server.
func (s *codeSink) fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	ch := make(chan sinkResult, 1)
	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return nil, err
	}
	s.current = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.current == ch {
			s.current = nil
		}
		s.mu.Unlock()
	}()

	// NEVER from a test binary (same guard as authflow): tests drive the callback directly,
	// and a real browser popping mid-`go test` is never wanted.
	if testing.Testing() {
		// no-op
	} else if err := openBrowser(args.URL); err != nil {
		// Not fatal — the user can paste the URL manually.
		fmt.Printf("  open this URL to authorize the MCP server:\n  %s\n", args.URL)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.result, r.err
	}
}

// openBrowser opens url in the user's default browser, per platform.
func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

// resolveHeaders runs a server's HeadersHelper (if any) and merges its output over the static
// headers, so a connection can mint a fresh token at connect time. The helper's stdout may be a
// JSON object of name→value, or "Name: value" lines.
func resolveHeaders(sc ServerConfig) (map[string]string, error) {
	headers := map[string]string{}
	for k, v := range sc.Headers {
		headers[k] = v
	}
	if strings.TrimSpace(sc.HeadersHelper) == "" {
		return headers, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", sc.HeadersHelper).Output()
	if err != nil {
		return nil, fmt.Errorf("headersHelper failed: %w", err)
	}
	for k, v := range parseHelperHeaders(out) {
		headers[k] = v
	}
	return headers, nil
}

// parseHelperHeaders reads a headers helper's stdout: a JSON object first, else "Name: value" lines.
func parseHelperHeaders(out []byte) map[string]string {
	headers := map[string]string{}
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(trimmed), &m) == nil {
			return m
		}
	}
	for _, line := range strings.Split(trimmed, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return headers
}
