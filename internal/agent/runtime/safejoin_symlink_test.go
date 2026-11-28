package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// A dangling symlink in the repo must not let a write land outside it: the
// link's target is checked even though nothing exists there yet.
func TestSafeJoinRejectsDanglingSymlinkOut(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "not-yet")
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skip("symlinks unsupported:", err)
	}
	if _, err := safeJoin(root, "evil"); err == nil {
		t.Fatal("write through a dangling symlink to outside the root was allowed")
	}
	if _, err := safeJoin(root, "evil/x.go"); err == nil {
		t.Fatal("write under a dangling symlinked dir outside the root was allowed")
	}
	// A cycle fails closed instead of recursing.
	_ = os.Symlink("loop", filepath.Join(root, "loop"))
	if _, err := safeJoin(root, "loop"); err == nil {
		t.Fatal("symlink cycle should be refused")
	}
	// An in-repo dangling link stays usable.
	_ = os.Symlink(filepath.Join(root, "later.go"), filepath.Join(root, "ok"))
	if _, err := safeJoin(root, "ok"); err != nil {
		t.Fatalf("in-repo dangling link refused: %v", err)
	}
}
