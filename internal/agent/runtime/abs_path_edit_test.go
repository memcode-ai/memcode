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

func absEditSess(t *testing.T, root string) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, captureProviderNil{}, root, "allow-all", permissions.ModeAllowAll, io.Discard)
}

// TestEditFileAbsoluteInRootPath: an ABSOLUTE path inside the repo must edit
// that file — not double-join under root into <root>/<root>/x.go.
func TestEditFileAbsoluteInRootPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "x.go")
	if err := os.WriteFile(path, []byte("package p\n\nvar A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := absEditSess(t, root)

	in, _ := json.Marshal(tools.EditFileInput{Path: path, OldString: "var A = 1", NewString: "var A = 9"})
	if r := s.editFile(context.Background(), in); r.isError {
		t.Fatalf("absolute in-root edit failed: %q", r.text())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "var A = 9") {
		t.Errorf("edit did not land in %s: %q", path, data)
	}
	// The double-join bug wrote a NEW file under <root>/<root>/… — make sure no
	// stray tree appeared inside the repo.
	if _, err := os.Stat(filepath.Join(root, root)); !os.IsNotExist(err) {
		t.Errorf("double-joined path %s exists (stat err %v)", filepath.Join(root, root), err)
	}
}

// TestApplyPatchAbsoluteInRootPath: same invariant for the atomic multi-edit path.
func TestApplyPatchAbsoluteInRootPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.go")
	if err := os.WriteFile(path, []byte("package p\nvar A = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := absEditSess(t, root)

	in, _ := json.Marshal(tools.ApplyPatchInput{Edits: []tools.EditFileInput{
		{Path: path, OldString: "var A = 1", NewString: "var A = 9"},
	}})
	if r := s.applyPatch(context.Background(), in); r.isError {
		t.Fatalf("absolute in-root apply_patch failed: %q", r.text())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "var A = 9") {
		t.Errorf("patch did not land in %s: %q", path, data)
	}
	if _, err := os.Stat(filepath.Join(root, root)); !os.IsNotExist(err) {
		t.Errorf("double-joined path %s exists (stat err %v)", filepath.Join(root, root), err)
	}
}
