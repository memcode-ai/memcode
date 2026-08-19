package runtime

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/agent/tools"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestStaleEditGuard: if a file changes on disk after memcode read it (another tool, the
// user, CI), editFile must REFUSE and tell the agent to re-read — not apply a stale edit.
func TestStaleEditGuard(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	path := filepath.Join(root, "x.go")
	os.WriteFile(path, []byte("package p\n\nvar A = 1\n"), 0o644)
	s := newSess(st, captureProviderNil{}, root, "allow-all", permissions.ModeAllowAll, io.Discard)

	rin, _ := json.Marshal(tools.ReadFileInput{Path: "x.go"})
	if r := s.readFile(context.Background(), rin); r.isError {
		t.Fatal("readFile errored")
	}
	os.WriteFile(path, []byte("package p\n\nvar A = 2\n"), 0o644) // another actor edits it

	ein, _ := json.Marshal(tools.EditFileInput{Path: "x.go", OldString: "var A = 1", NewString: "var A = 9"})
	r := s.editFile(ctx, ein)
	out, isErr := r.text(), r.isError
	if !isErr || !strings.Contains(out, "changed on disk") {
		t.Fatalf("stale edit not refused: isErr=%v out=%q", isErr, out)
	}

	// Re-read the fresh content → the edit now applies.
	s.readFile(context.Background(), rin)
	ein2, _ := json.Marshal(tools.EditFileInput{Path: "x.go", OldString: "var A = 2", NewString: "var A = 9"})
	if r := s.editFile(ctx, ein2); r.isError {
		t.Fatalf("edit after re-read should apply, got error: %q", r.text())
	}
}

// TestBatchEditsSameFileDontTrip: several edits to the SAME file in one turn (with no
// external change) must all apply — the guard updates its recorded hash after each edit,
// so memcode's own successive edits never look "stale" to it.
func TestBatchEditsSameFileDontTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.go"), []byte("package p\n\nvar A = 1\n"), 0o644)
	s := newSess(st, captureProviderNil{}, root, "allow-all", permissions.ModeAllowAll, io.Discard)

	rin, _ := json.Marshal(tools.ReadFileInput{Path: "x.go"})
	s.readFile(context.Background(), rin)
	for _, e := range [][2]string{{"var A = 1", "var A = 2"}, {"var A = 2", "var A = 3"}, {"var A = 3", "var A = 4"}} {
		ein, _ := json.Marshal(tools.EditFileInput{Path: "x.go", OldString: e[0], NewString: e[1]})
		if r := s.editFile(ctx, ein); r.isError {
			t.Fatalf("batch edit %v wrongly refused: %q", e, r.text())
		}
	}
}
