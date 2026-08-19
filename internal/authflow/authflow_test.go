package authflow

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCallbackStateValidation verifies the local callback handler ignores a
// mismatched state parameter (a stray local request must not abort the
// pending login) and accepts a matching one afterwards.
func TestCallbackStateValidation(t *testing.T) {
	const state = "abc123"
	resultCh := make(chan loginResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", makeCallbackHandler(state, resultCh))

	server := httptest.NewServer(mux)
	defer server.Close()

	// 1. Mismatched state → ignored (404), nothing delivered, login still live.
	res, err := http.Get(server.URL + "/callback?state=wrong&token=memcode_xyz")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("mismatched state: expected 404, got %d", res.StatusCode)
	}
	select {
	case r := <-resultCh:
		t.Errorf("stray request must not deliver a result, got %+v", r)
	default:
	}

	// 2. Matching state + token → success (the flow survived the stray hit).
	res, err = http.Get(server.URL + "/callback?state=" + state + "&token=memcode_testtoken&api_url=https://api.memcode.ai")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("valid callback: expected 200, got %d", res.StatusCode)
	}
	r := <-resultCh
	if r.token != "memcode_testtoken" {
		t.Errorf("expected token 'memcode_testtoken', got %q", r.token)
	}
	if r.apiURL != "https://api.memcode.ai" {
		t.Errorf("expected api_url 'https://api.memcode.ai', got %q", r.apiURL)
	}
}

// TestCallbackPostForm verifies the forward-compatible delivery path: the token in a POST
// form body (kept out of the URL entirely), and the token-hygiene response headers.
func TestCallbackPostForm(t *testing.T) {
	const state = "abc123"
	resultCh := make(chan loginResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", makeCallbackHandler(state, resultCh))
	server := httptest.NewServer(mux)
	defer server.Close()

	form := url.Values{"state": {state}, "token": {"memcode_posted"}, "api_url": {"https://api.memcode.ai"}}
	res, err := http.PostForm(server.URL+"/callback", form)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("POST callback: expected 200, got %d", res.StatusCode)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if rp := res.Header.Get("Referrer-Policy"); rp != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", rp)
	}
	r := <-resultCh
	if r.token != "memcode_posted" || r.apiURL != "https://api.memcode.ai" {
		t.Errorf("posted result = %+v", r)
	}
}

// TestWriteAndStripGlobalEnvToken round-trips the real file logic against a
// temp XDG config home: write replaces old values, strip removes them and
// reports whether anything changed.
func TestWriteAndStripGlobalEnvToken(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "memcode", ".env")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	seed := "# memcode config\nMEMCODE_API_URL=https://old.example.com\nMEMCODE_API_TOKEN=memcode_old\nOTHER=keep\n"
	if err := os.WriteFile(path, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}

	if err := WriteGlobalEnvToken("memcode_new", "https://api.memcode.ai"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Contains(s, "memcode_old") {
		t.Error("old token was not stripped")
	}
	for _, want := range []string{"MEMCODE_API_TOKEN=memcode_new", "MEMCODE_API_URL=https://api.memcode.ai", "# memcode config", "OTHER=keep"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in written file:\n%s", want, s)
		}
	}

	removed, err := StripGlobalEnvToken()
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("strip should report removal")
	}
	got, _ = os.ReadFile(path)
	s = string(got)
	if strings.Contains(s, "MEMCODE_API_TOKEN") || strings.Contains(s, "MEMCODE_API_URL") {
		t.Errorf("token lines survived strip:\n%s", s)
	}
	if !strings.Contains(s, "OTHER=keep") {
		t.Error("unrelated line was dropped by strip")
	}

	// Second strip: nothing left to remove.
	removed, err = StripGlobalEnvToken()
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("second strip should be a no-op")
	}
}

// TestRunCancel verifies ctx cancellation aborts the flow promptly (the TUI's
// Esc path) instead of blocking on the 2-minute timeout. Browser opening is
// best-effort and may fail in CI — irrelevant here.
func TestRunCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, nil)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the listener start
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled Run must return an error")
		}
		if !strings.Contains(err.Error(), "cancel") && !strings.Contains(err.Error(), "19090") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestCallbackQueryParsing ensures the callback URL is parsed correctly for
// edge cases (empty token, error param).
func TestCallbackQueryParsing(t *testing.T) {
	v, _ := url.Parse("http://localhost:19090/callback?token=&state=xyz&error=denied")
	if v.Query().Get("error") != "denied" {
		t.Error("failed to parse error param")
	}
	if v.Query().Get("token") != "" {
		t.Error("empty token should parse as empty string")
	}
}
