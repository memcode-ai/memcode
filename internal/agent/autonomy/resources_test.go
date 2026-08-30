package autonomy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResourceGrantCanonicalBoundaryAndExpiration(t *testing.T) {
	root := t.TempDir()
	canonical, err := CanonicalFilesystemGrant(root)
	if err != nil {
		t.Fatal(err)
	}
	if !PathWithinGrant(filepath.Join(canonical, "child"), canonical) {
		t.Fatal("granted child denied")
	}
	if PathWithinGrant(filepath.Dir(canonical), canonical) {
		t.Fatal("path outside grant allowed")
	}
	now := time.Date(2026, time.August, 30, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	g := ResourceGrantModel{Status: "active", ExpiresAt: &future}
	if !g.Active(now) {
		t.Fatal("active grant denied")
	}
	past := now.Add(-time.Hour)
	g.ExpiresAt = &past
	if g.Active(now) {
		t.Fatal("expired grant allowed")
	}
}

// Regression: a symlink inside a granted dir pointing outside must NOT satisfy
// the grant (Codex P0). PathWithinGrant resolves the requested path's symlinks.
func TestPathWithinGrantRejectsSymlinkEscape(t *testing.T) {
	grant := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("s3cret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlink inside the grant pointing to the outside file.
	link := filepath.Join(grant, "escape.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if PathWithinGrant(link, grant) {
		t.Fatal("symlink to outside path was treated as within grant")
	}
	// A symlinked DIRECTORY inside the grant pointing outside must also fail.
	linkDir := filepath.Join(grant, "out")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	if PathWithinGrant(filepath.Join(linkDir, "secret.txt"), grant) {
		t.Fatal("symlinked dir escape treated as within grant")
	}
	// A genuine in-grant path still passes.
	real := filepath.Join(grant, "real.txt")
	if err := os.WriteFile(real, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !PathWithinGrant(real, grant) {
		t.Fatal("in-grant path rejected")
	}
}
