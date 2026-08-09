package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestFetchTool(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, captureProviderNil{}, t.TempDir(), "sonnet", permissions.ModeAsk, io.Discard)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html><head><style>x{}</style></head><body><h1>Title</h1><p>Hello &amp; world</p><script>bad()</script></body></html>")
		case "/data":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()

	// HTML → visible text only (no script/style/tags), entities decoded.
	r := s.fetchTool(ctx, []byte(`{"url":"`+srv.URL+`/page"}`))
	out, isErr := r.text(), r.isError
	if isErr {
		t.Fatalf("fetch html errored: %q", out)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Hello & world") {
		t.Fatalf("html text missing: %q", out)
	}
	if strings.Contains(out, "bad()") || strings.Contains(out, "<") {
		t.Fatalf("html not stripped: %q", out)
	}
	// JSON passes through.
	r = s.fetchTool(ctx, []byte(`{"url":"`+srv.URL+`/data"}`))
	out, isErr = r.text(), r.isError
	if isErr || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("json fetch: %q (err=%v)", out, isErr)
	}
	// Metadata endpoint is refused.
	r = s.fetchTool(ctx, []byte(`{"url":"http://169.254.169.254/latest/meta-data/"}`))
	out, isErr = r.text(), r.isError
	if !isErr || !strings.Contains(out, "metadata") {
		t.Fatalf("metadata endpoint must be refused, got %q (err=%v)", out, isErr)
	}
	// Missing url.
	if r := s.fetchTool(ctx, []byte(`{}`)); !r.isError {
		t.Fatal("empty fetch should error")
	}
}
