package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// safeJoin must reject a write whose real path escapes the repo via an in-repo symlink — a
// lexical prefix check alone (the old behavior) let `./link → /` redirect the write outside.
func TestSafeJoinRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := safeJoin(root, "escape/secret"); err == nil {
		t.Fatal("safeJoin should reject a path through an in-repo symlink pointing outside root")
	}
	// A normal in-repo path (including a not-yet-existent file under a real dir) still resolves.
	if _, err := safeJoin(root, "sub/new.go"); err != nil {
		t.Fatalf("safeJoin should allow a normal in-repo path: %v", err)
	}
	// A lexical escape is still rejected too.
	if _, err := safeJoin(root, "../../etc/passwd"); err == nil {
		t.Fatal("safeJoin should reject a lexical ../ escape")
	}
}

// Absolute paths: honored when inside the root, rejected with the honest
// "escapes" error when outside. The old behavior JOINED an absolute path under
// root ("<root>/Users/x/Desktop"), a nonexistent path that passed the prefix
// check and then failed downstream as "fork/exec /bin/sh: no such file or
// directory" (the chdir errno blamed on the shell) — the 2026-07-18 pitch-deck
// session hit exactly this via a bash cwd of /Users/…/Desktop.
func TestSafeJoinAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := safeJoin(root, sub) // absolute, inside root
	if err != nil {
		t.Fatalf("absolute in-root path rejected: %v", err)
	}
	if fi, statErr := os.Stat(got); statErr != nil || !fi.IsDir() {
		t.Fatalf("absolute in-root path resolved to a nonexistent dir: %q", got)
	}

	if _, err := safeJoin(root, "/Users/nobody/Desktop"); err == nil {
		t.Fatal("absolute out-of-root path must error, not be silently joined under root")
	}
}
