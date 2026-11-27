package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The layout fact maps the top level deterministically: separate repos, empty
// folders and sizes, hidden entries skipped. The case that motivated it: an
// empty folder sitting among unrelated repos.
func TestLayoutFact(t *testing.T) {
	root := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(filepath.Join(root, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("memcode/.git")
	mk("memcode/cmd")
	mk("memcode/internal")
	mk("halo/.git")
	mk("halo/sdk")
	mk("moonlight")
	mk(".memcode")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"halo/ — git repo · 1 entry",
		"memcode/ — git repo · 2 entries",
		"moonlight/ — empty",
		"files: README.md",
	}, "\n")
	if got := layoutFact(root); got != want {
		t.Fatalf("layout =\n%s\nwant\n%s", got, want)
	}
	if got := layoutFact(t.TempDir()); got != "(empty)" {
		t.Fatalf("empty root layout = %q", got)
	}
}

// A huge top level is capped so the fact stays a glance.
func TestLayoutFactCapped(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < layoutMaxEntries+5; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("d%02d", i)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got := layoutFact(root)
	if n := strings.Count(got, "\n") + 1; n != layoutMaxEntries+1 || !strings.HasSuffix(got, "… and 5 more") {
		t.Fatalf("capped layout has %d lines, tail %q", n, got[len(got)-20:])
	}
}
