package repofiles

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDropVendored(t *testing.T) {
	in := []string{
		"go.mod",
		"cmd/main.go",
		"internal/agent/runtime.go",
		"internal/forks/vaxis/vaxis.go", // vendored fork → dropped
		"internal/forks/vaxis/ui/flex.go",
		"vendor/dep/dep.go",      // classic vendor/ → dropped
		"services/api/server.go", // first-party submodule → KEPT (not vendored)
	}
	got := dropVendored(in)
	want := []string{"go.mod", "cmd/main.go", "internal/agent/runtime.go", "services/api/server.go"}
	if !slices.Equal(got, want) {
		t.Fatalf("dropVendored = %v, want %v (only vendored trees dropped; first-party submodule kept)", got, want)
	}
}

func TestDropVendoredNoVendor(t *testing.T) {
	in := []string{"go.mod", "a.go", "pkg/b.go"}
	if got := dropVendored(in); !slices.Equal(got, in) {
		t.Fatalf("with no vendored trees the list is unchanged; got %v", got)
	}
}

// The non-git fallback walk must be bounded: nothing below walkMaxDepth, and a
// cancelled context stops the walk — a first run in $HOME must never crawl the
// world before the prompt appears.
func TestWalkListBounded(t *testing.T) {
	root := t.TempDir()

	// A file at depth 2 (kept) and one below walkMaxDepth (dropped).
	shallow := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(shallow, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shallow, "keep.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	deepParts := append([]string{root}, strings.Split(strings.Repeat("d/", walkMaxDepth+2), "/")...)
	deep := filepath.Join(deepParts...)
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "toodeep.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	files := walkList(context.Background(), root)
	if !slices.Contains(files, "a/b/keep.txt") {
		t.Errorf("shallow file missing from walk: %v", files)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "toodeep.txt") {
			t.Errorf("file below walkMaxDepth was listed: %s", f)
		}
	}

	// Cancelled context → immediate stop, empty-ish result, no hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := walkList(ctx, root); len(got) != 0 {
		t.Errorf("cancelled walk returned files: %v", got)
	}
}
