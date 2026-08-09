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

func applyPatchSess(t *testing.T, root string) *Session {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return newSess(st, captureProviderNil{}, root, "allow-all", permissions.ModeAllowAll, io.Discard)
}

// A multi-file patch where every edit is valid applies them ALL.
func TestApplyPatchAllOrNothing_Success(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\nvar A = 1\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package p\nvar B = 1\n"), 0o644)
	s := applyPatchSess(t, root)

	in, _ := json.Marshal(tools.ApplyPatchInput{Edits: []tools.EditFileInput{
		{Path: "a.go", OldString: "var A = 1", NewString: "var A = 9"},
		{Path: "b.go", OldString: "var B = 1", NewString: "var B = 9"},
	}})
	if r := s.applyPatch(context.Background(), in); r.isError {
		t.Fatalf("valid patch should apply: %q", r.text())
	}
	for _, f := range []string{"a.go", "b.go"} {
		b, _ := os.ReadFile(filepath.Join(root, f))
		if !strings.Contains(string(b), "= 9") {
			t.Errorf("%s not updated: %q", f, b)
		}
	}
}

// If ANY edit fails, the whole patch rolls back — no file is left half-changed.
func TestApplyPatchAllOrNothing_Rollback(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\nvar A = 1\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package p\nvar B = 1\n"), 0o644)
	s := applyPatchSess(t, root)

	// Second edit's old_string doesn't exist → the batch must fail and roll back the first.
	in, _ := json.Marshal(tools.ApplyPatchInput{Edits: []tools.EditFileInput{
		{Path: "a.go", OldString: "var A = 1", NewString: "var A = 9"},
		{Path: "b.go", OldString: "NONEXISTENT", NewString: "var B = 9"},
	}})
	r := s.applyPatch(context.Background(), in)
	if !r.isError {
		t.Fatal("patch with a bad edit must fail")
	}
	// a.go must be UNCHANGED (rolled back), b.go untouched.
	a, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if strings.Contains(string(a), "= 9") {
		t.Fatalf("a.go must be rolled back, got %q", a)
	}
	if !strings.Contains(string(a), "var A = 1") {
		t.Fatalf("a.go must be restored to original, got %q", a)
	}
}

// A created file is rolled back by DELETION when a later edit fails.
func TestApplyPatchRollbackDeletesCreated(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package p\nvar A = 1\n"), 0o644)
	s := applyPatchSess(t, root)

	in, _ := json.Marshal(tools.ApplyPatchInput{Edits: []tools.EditFileInput{
		{Path: "new.go", OldString: "", NewString: "package p\n"}, // create
		{Path: "a.go", OldString: "NOPE", NewString: "x"},         // fails → roll back
	}})
	if r := s.applyPatch(context.Background(), in); !r.isError {
		t.Fatal("patch must fail")
	}
	if _, err := os.Stat(filepath.Join(root, "new.go")); !os.IsNotExist(err) {
		t.Fatal("a file created by a rolled-back patch must be deleted")
	}
}
