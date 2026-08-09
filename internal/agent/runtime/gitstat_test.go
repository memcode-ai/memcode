package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestSessionGitStat(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "base")

	// Store OUTSIDE the repo so its db doesn't dirty the tree.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, &captureProvider{}, dir, "sonnet", permissions.ModeAsk, nil)

	// Clean tree.
	if g := s.GitStat(ctx); !g.Clean() {
		t.Fatalf("clean tree should report clean, got %+v", g)
	}

	// Modify a tracked file (+1 line) and add an untracked file (2 lines).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x\ny\n"), 0o644)
	g := s.GitStat(ctx)
	if g.Clean() {
		t.Fatal("dirty tree reported clean")
	}
	if g.Files != 2 { // a.txt (modified) + new.txt (untracked)
		t.Errorf("Files = %d, want 2 (modified + untracked): %+v", g.Files, g)
	}
	if g.Added < 3 { // +1 in a.txt, +2 in new.txt
		t.Errorf("Added = %d, want ≥3: %+v", g.Added, g)
	}

	// Stage the tracked change → it moves to the staged columns.
	gitCmd(t, dir, "add", "a.txt")
	g = s.GitStat(ctx)
	if g.StagedFiles < 1 {
		t.Errorf("staged change not reflected: %+v", g)
	}
}
