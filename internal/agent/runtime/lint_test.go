package runtime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestValidateEditGo(t *testing.T) {
	good := "package foo\n\nfunc Bar() int { return 1 }\n"
	if w := validateEdit("foo.go", good); w != "" {
		t.Errorf("valid Go flagged: %q", w)
	}
	// A botched edit: unbalanced brace.
	bad := "package foo\n\nfunc Bar() int { return 1 \n"
	w := validateEdit("foo.go", bad)
	if w == "" {
		t.Fatal("broken Go not flagged")
	}
	if !strings.Contains(w, "foo.go") || !strings.Contains(strings.ToLower(w), "syntax error") {
		t.Errorf("warning should name the file + syntax error, got: %q", w)
	}
}

func TestValidateEditNonGoIsClean(t *testing.T) {
	// Non-Go files aren't checked (no false positives until we add their lexers).
	if w := validateEdit("notes.md", "# heading\n```go\nbroken{\n```"); w != "" {
		t.Errorf("markdown should not be syntax-checked, got: %q", w)
	}
	if w := validateEdit("data.json", "{not valid json"); w != "" {
		t.Errorf("json not yet checked, should be clean, got: %q", w)
	}
}

func TestValidateGoCapsErrorFlood(t *testing.T) {
	// Many cascading errors shouldn't flood the model.
	bad := "package foo\nfunc (((( \n}}}}\n))))\n garbage garbage garbage\n"
	w := validateEdit("x.go", bad)
	if w == "" {
		t.Fatal("expected errors")
	}
	if n := strings.Count(w, "\n"); n > 6 {
		t.Errorf("error output not capped (%d lines): %q", n, w)
	}
}

func TestBrokenEditNudge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, ".state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s.turn.editedPaths = map[string]bool{}

	// A clean edited file → no nudge.
	if err := os.WriteFile(filepath.Join(dir, "ok.go"), []byte("package p\nfunc F(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.turn.editedPaths["ok.go"] = true
	if n := s.brokenEditNudge(); n != "" {
		t.Errorf("clean file should not nudge: %q", n)
	}

	// Break a second edited file → nudge names it and tells the model to fix it.
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package p\nfunc F(){\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.turn.editedPaths["bad.go"] = true
	n := s.brokenEditNudge()
	if n == "" || !strings.Contains(n, "bad.go") {
		t.Fatalf("broken file should nudge and name bad.go, got: %q", n)
	}
	if strings.Contains(n, "ok.go") {
		t.Errorf("clean file should not appear in nudge: %q", n)
	}

	// Model fixes it → nudge clears (validated against current disk content).
	if err := os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package p\nfunc F(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := s.brokenEditNudge(); n != "" {
		t.Errorf("after fix, nudge should clear: %q", n)
	}
}
