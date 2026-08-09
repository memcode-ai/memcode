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
// returned closer releases the listener (call it when the connection is torn down). httpClient
// is used for the SDK's metadata/registration/token requests.
func oauthHandler(httpClient *http.Client) (auth.OAuthHandler, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, func() {}, err
	}
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())
	closer := func() { _ = ln.Close() }

	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				RedirectURIs: []string{redirect},
				ClientName:   "memcode",
			},
		},
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: loopbackFetcher(ln),
		Client:                   httpClient,
	})
	if err != nil {
		closer()
		return nil, func() {}, err
	}
	return h, closer, nil
}

// loopbackFetcher opens the authorization URL in the user's browser and serves a single request
// on ln to capture the redirected code/state.
func loopbackFetcher(ln net.Listener) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		if err := openBrowser(args.URL); err != nil {
			// Not fatal — the user can paste the URL manually.
			fmt.Printf("  open this URL to authorize the MCP server:\n  %s\n", args.URL)
		}
		type res struct {
			result *auth.AuthorizationResult
			err    error
		}
		ch := make(chan res, 1)
		srv := &http.Server{}
		srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
				ch <- res{nil, fmt.Errorf("authorization error: %s", e)}
				return
			}
			code := q.Get("code")
			if code == "" {
				return // ignore favicon and other stray hits
			}
			_, _ = w.Write([]byte("<html><body>Authorized. You can close this tab and return to memcode.</body></html>"))
			ch <- res{&auth.AuthorizationResult{Code: code, State: q.Get("state")}, nil}
		})
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case r := <-ch:
			return r.result, r.err
		}
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
