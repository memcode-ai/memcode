package input

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBase64Len(t *testing.T) {
	for _, c := range []struct{ in, want int64 }{{0, 0}, {1, 4}, {3, 4}, {4, 8}, {6, 8}} {
		if got := Base64Len(c.in); got != c.want {
			t.Errorf("Base64Len(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestCapAttachments: the bundle is bounded by COUNT and by AGGREGATE BASE64 bytes (the
// encoded size that actually travels — not raw file size).
func TestCapAttachments(t *testing.T) {
	// Count cap: MaxAttachments+5 tiny files → only MaxAttachments kept.
	var many []Attachment
	for i := 0; i < MaxAttachments+5; i++ {
		many = append(many, Attachment{Path: "x", SizeBytes: 1})
	}
	if kept, dropped := CapAttachments(many); len(kept) != MaxAttachments || dropped != 5 {
		t.Fatalf("count cap: kept=%d dropped=%d, want %d/5", len(kept), dropped, MaxAttachments)
	}
	// Aggregate base64 cap: budget = 32MB-4MB = 28MB encoded. Each 8MB raw → ~10.7MB base64;
	// two fit (~21.3MB), the third (~32MB) doesn't → 2 kept, 1 dropped. Raw 8MB×3 = 24MB would
	// PASS a naive raw-bytes check — which is exactly why the check is base64-aware.
	big := []Attachment{
		{Path: "a", SizeBytes: 8 << 20},
		{Path: "b", SizeBytes: 8 << 20},
		{Path: "c", SizeBytes: 8 << 20},
	}
	if kept, dropped := CapAttachments(big); len(kept) != 2 || dropped != 1 {
		t.Fatalf("base64 byte cap: kept=%d dropped=%d, want 2/1", len(kept), dropped)
	}
}

// TestHugeFileSkipsHash: a file over maxHashBytes is fingerprinted-skipped (no full read).
// Uses a SPARSE file (os.Truncate) so the test stays instant and writes no real gigabytes.
func TestHugeFileSkipsHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, maxHashBytes+1); err != nil { // sparse grow
		t.Fatal(err)
	}
	a, ok := Resolve(p, dir, "drag_drop")
	if !ok || a.SizeBytes <= maxHashBytes {
		t.Fatalf("setup: ok=%v size=%d", ok, a.SizeBytes)
	}
	if a.SHA256 != "" {
		t.Fatalf("expected hashing skipped for a >100MB file, got sha %q", a.SHA256)
	}
}

// TestHeicNotExtracted: an unsupported image type (HEIC) is not matched/attached — it stays text.
func TestHeicNotExtracted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.heic")
	if err := os.WriteFile(p, []byte("ftypheic and some bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m := ImageMatches(p, dir); len(m) != 0 {
		t.Fatalf("HEIC should not match as an image path: %v", m)
	}
	if d := Parse(p, dir); len(d.Bundle.Attachments) != 0 {
		t.Fatalf("HEIC should not become an attachment, got %d", len(d.Bundle.Attachments))
	}
}

// TestResolveOutsideCwdAllowed pins TODAY's path policy: an absolute path OUTSIDE cwd is
// honored for an explicit drag/paste. If a future workspace-confinement policy changes this,
// it should be a deliberate edit to this test, not an accident.
func TestResolveOutsideCwdAllowed(t *testing.T) {
	outside := t.TempDir() // a dir unrelated to cwd
	cwd := t.TempDir()
	p := filepath.Join(outside, "shot.png")
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, outside) {
		t.Fatal("setup")
	}
	if a, ok := Resolve(p, cwd, "drag_drop"); !ok || a.Kind != KindImage {
		t.Fatalf("absolute out-of-cwd image should resolve today: ok=%v kind=%q", ok, a.Kind)
	}
}
