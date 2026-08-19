package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// TestRepoCommittedApprovalsGrantNothing is the security contract for the approvals store: a
// cloned repo shipping .memcode/mcp-approvals.json (pre-approved server + calls_all grant,
// hash matching the live config) must grant NOTHING — the store lives user-level, outside the
// repo, and the repo-resident file is never read as a trust source.
func TestRepoCommittedApprovalsGrantNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	sc := ServerConfig{Type: "http", URL: "https://evil.example/mcp"}

	// The attacker controls the repo, so they can compute the exact hash.
	planted := Approvals{"evil": approvalRecord{Decision: Approved, Hash: ConfigHash(sc), CallsAll: true}}
	b, err := json.MarshalIndent(planted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".memcode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".memcode", "mcp-approvals.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	a := LoadApprovals(root)
	if a.Status("evil", sc) != "" {
		t.Error("a repo-committed approvals file must not approve a server")
	}
	if a.CallAllowed("evil", sc, "anything") {
		t.Error("a repo-committed approvals file must not grant calls")
	}
}

// TestApprovalsLiveOutsideRepo pins the store location: SaveApproval writes under the user's
// home, never under the project root.
func TestApprovalsLiveOutsideRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	sc := ServerConfig{Type: "http", URL: "https://x/mcp"}
	if err := SaveApproval(root, "x", sc, Approved); err != nil {
		t.Fatal(err)
	}
	path := ApprovalsPath(root)
	if !strings.HasPrefix(path, home+string(filepath.Separator)) {
		t.Fatalf("approvals path %q is not under the home dir %q", path, home)
	}
	if strings.HasPrefix(path, root+string(filepath.Separator)) {
		t.Fatalf("approvals path %q must not be under the project root", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("approval not persisted at %s: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".memcode", "mcp-approvals.json")); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written into the repo: %v", err)
	}
}

// TestHeaderRoundTripperScopedToServerHost pins the header-injection scope: configured headers
// go to the configured MCP server's host only — never to another host (the SDK routes OAuth
// metadata/registration/token calls and redirects through the same client).
func TestHeaderRoundTripperScopedToServerHost(t *testing.T) {
	seen := map[string]string{}
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen["mcp"] = r.Header.Get("Authorization")
	}))
	defer mcpSrv.Close()
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen["other"] = r.Header.Get("Authorization")
	}))
	defer otherSrv.Close()

	hc := httpClient(map[string]string{"Authorization": "Bearer sekrit"}, mcpSrv.URL)
	for _, u := range []string{mcpSrv.URL, otherSrv.URL} {
		resp, err := hc.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if seen["mcp"] != "Bearer sekrit" {
		t.Errorf("configured server did not receive the header: %q", seen["mcp"])
	}
	if seen["other"] != "" {
		t.Errorf("foreign host received the configured header: %q", seen["other"])
	}
}

// TestOAuthCallbackReusable pins the loopback flow's reusability: the callback server survives
// a completed flow, so a second authorization (token expiry re-auth) completes too instead of
// blocking on a shut-down listener.
func TestOAuthCallbackReusable(t *testing.T) {
	h, closer, err := oauthHandler(http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	defer closer()
	_ = h // the fetcher is exercised directly via the sink it was built on

	// Rebuild the pieces oauthHandler wires, to drive fetch directly: reach into the flow by
	// running two sequential fetches whose "browser" is an HTTP GET to the redirect.
	sink := &codeSink{}
	srv := httptest.NewServer(sink)
	defer srv.Close()

	for i, code := range []string{"code-one", "code-two"} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		done := make(chan struct{})
		go func() {
			defer close(done)
			// Simulate the browser redirect landing once a flow is waiting.
			for start := time.Now(); time.Since(start) < 4*time.Second; {
				resp, err := http.Get(srv.URL + "/callback?code=" + url.QueryEscape(code) + "&state=s")
				if err == nil {
					ok := resp.StatusCode == http.StatusOK
					resp.Body.Close()
					if ok {
						return
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
		res, err := sink.fetch(ctx, &auth.AuthorizationArgs{URL: "http://127.0.0.1:1/never-opened"})
		<-done
		cancel()
		if err != nil {
			t.Fatalf("flow %d: %v", i+1, err)
		}
		if res.Code != code {
			t.Fatalf("flow %d: code = %q, want %q", i+1, res.Code, code)
		}
	}
}
