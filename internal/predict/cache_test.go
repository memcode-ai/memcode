package predict

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/store"
)

func TestFingerprintSensitivity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir() // not a repo → Head() == "", isolates the working-tree signal

	a := Fingerprint(ctx, root, Evidence{DirtyFiles: []string{"a.go"}, Diff: "x"})
	same := Fingerprint(ctx, root, Evidence{DirtyFiles: []string{"a.go"}, Diff: "x"})
	diffFile := Fingerprint(ctx, root, Evidence{DirtyFiles: []string{"b.go"}, Diff: "x"})
	diffDiff := Fingerprint(ctx, root, Evidence{DirtyFiles: []string{"a.go"}, Diff: "y"})

	if a != same {
		t.Fatal("identical evidence must fingerprint identically")
	}
	if a == diffFile || a == diffDiff {
		t.Fatal("changed working tree must change the fingerprint")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := t.TempDir()
	fp := Fingerprint(ctx, root, Evidence{DirtyFiles: []string{"a.go"}})

	if _, fresh := LoadCached(ctx, st, root, fp); fresh {
		t.Fatal("empty cache must not be fresh")
	}
	StoreCached(ctx, st, root, fp, "where you were: x")
	got, fresh := LoadCached(ctx, st, root, fp)
	if !fresh || got.Text != "where you were: x" {
		t.Fatalf("round-trip failed: fresh=%v text=%q", fresh, got.Text)
	}
	// A different working-tree fingerprint invalidates.
	if _, fresh := LoadCached(ctx, st, root, fp+"x"); fresh {
		t.Fatal("a changed fingerprint must miss the cache")
	}
}
