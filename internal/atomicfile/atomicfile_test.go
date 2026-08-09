package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreatesOverwritesAndSetsPerm(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")

	if err := WriteFile(p, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "first" {
		t.Fatalf("content = %q, want first", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", fi.Mode().Perm())
	}

	// Overwrite in place.
	if err := WriteFile(p, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "second" {
		t.Fatalf("content = %q, want second", b)
	}

	// No stray temp files remain in the directory.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected only the target file, got %d entries: %v", len(entries), entries)
	}
}
