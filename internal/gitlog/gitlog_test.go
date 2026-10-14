package gitlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// initRepo creates a git repo at dir with one commit, so it has real history
// for Recent/Roots to find.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# "+filepath.Base(dir)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-q", "-m", "initial commit in "+filepath.Base(dir))
}

func TestRootsSingleRepoUnchanged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	initRepo(t, root)

	roots := Roots(ctx, root)
	if len(roots) != 1 {
		t.Fatalf("Roots = %v, want exactly one root", roots)
	}

	// Commits found via path="" must be identical whether queried directly
	// or through Recent's whole-repo branch — the common case is untouched.
	direct := logRecords(ctx, root, "-n", "10")
	viaRecent := Recent(ctx, root, "", 10)
	if len(direct) != 1 || len(viaRecent) != 1 {
		t.Fatalf("direct=%d viaRecent=%d, want 1 each", len(direct), len(viaRecent))
	}
	if direct[0].Hash != viaRecent[0].Hash || direct[0].Subject != viaRecent[0].Subject {
		t.Fatalf("Recent diverged from direct log: %+v vs %+v", direct[0], viaRecent[0])
	}
}

func TestRootsNoGitAnywhere(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}

	if roots := Roots(ctx, root); len(roots) != 0 {
		t.Fatalf("Roots = %v, want none", roots)
	}
	if commits := Recent(ctx, root, "", 10); len(commits) != 0 {
		t.Fatalf("Recent = %v, want none", commits)
	}
}

func TestRootsBoundedRecursiveDiscovery(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()

	// Sibling repos at varying depths under one non-git parent.
	depth0 := filepath.Join(parent, "depth0")
	depth1 := filepath.Join(parent, "nested", "depth1")
	depth3 := filepath.Join(parent, "nested", "a", "b", "depth3")
	initRepo(t, depth0)
	initRepo(t, depth1)
	initRepo(t, depth3)

	// A decoy tree without a .git dir, inside an excluded directory — must
	// never be descended into, let alone mistaken for a root.
	decoy := filepath.Join(parent, "nested", "node_modules", "some-pkg")
	if err := os.MkdirAll(decoy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "index.js"), []byte("module.exports = {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots := Roots(ctx, parent)
	want := []string{depth0, depth1, depth3}
	sort.Strings(want)
	if len(roots) != len(want) {
		t.Fatalf("Roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("Roots = %v, want %v", roots, want)
		}
	}

	// Recent("") merges commits across all discovered roots by date.
	commits := Recent(ctx, parent, "", 10)
	if len(commits) != 3 {
		t.Fatalf("Recent merged = %d commits, want 3: %+v", len(commits), commits)
	}
}

func TestRootsCapsTerminate(t *testing.T) {
	ctx := context.Background()
	parent := t.TempDir()

	// A deep, repo-free chain well past rootsMaxDepth — discovery must
	// terminate (not hang) and find nothing.
	deep := parent
	for i := 0; i < rootsMaxDepth+10; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	done := make(chan []string, 1)
	go func() { done <- Roots(ctx, parent) }()
	select {
	case roots := <-done:
		if len(roots) != 0 {
			t.Fatalf("Roots = %v, want none (too deep)", roots)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Roots did not terminate within the depth cap")
	}
}
