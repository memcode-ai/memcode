package overview

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/llm"
	"github.com/memcode-ai/memcode/internal/store"
	"github.com/memcode-ai/memcode/internal/wire"
)

// countProvider returns canned text and counts how many model calls happen.
type countProvider struct {
	text string
	n    int
}

func (c *countProvider) Complete(ctx context.Context, r wire.Request) (wire.Response, error) {
	c.n++
	return wire.Response{StopReason: "end_turn", Blocks: []wire.Block{{Type: "text", Text: c.text}}}, nil
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestOverviewCacheAndFreshness(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The overview is now a DETERMINISTIC briefing (no model call) — assert the cache +
	// freshness contract, not a model-call count.
	cp := &countProvider{text: "unused"}
	o, err := Synthesize(ctx, st, llm.NewRunner(cp), dir, "sonnet")
	if err != nil || strings.TrimSpace(o.Text) == "" {
		t.Fatalf("synthesize: %+v err=%v", o, err)
	}
	want := o.Text

	// Cached + fresh (HEAD unchanged) ⇒ Get reuses the cached briefing.
	if loaded, fresh := Load(ctx, st, dir); !fresh || loaded.Text != want {
		t.Fatalf("expected fresh cache, got fresh=%v", fresh)
	}
	if g, err := Get(ctx, st, llm.NewRunner(cp), dir, "sonnet"); err != nil || g.Text != want {
		t.Fatalf("Get should reuse the cached overview, err=%v", err)
	}

	// A new commit moves HEAD ⇒ cache is stale ⇒ Get re-synthesizes.
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644)
	git(t, dir, "commit", "-aqm", "change")
	if _, fresh := Load(ctx, st, dir); fresh {
		t.Fatal("cache should be stale after a new commit")
	}
	if g, err := Get(ctx, st, llm.NewRunner(cp), dir, "sonnet"); err != nil || strings.TrimSpace(g.Text) == "" {
		t.Fatalf("Get should re-synthesize after HEAD moved, err=%v", err)
	}
}

// TestOverviewInvalidatesOnDirtyState is the trust regression: an UNCOMMITTED edit
// (HEAD unchanged) must invalidate the overview, and so must committing it. Git
// state — not time — is the freshness boundary.
func TestOverviewInvalidatesOnDirtyState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := Synthesize(ctx, st, llm.NewRunner(&countProvider{text: "ov"}), dir, "sonnet"); err != nil {
		t.Fatal(err)
	}
	if _, fresh := Load(ctx, st, dir); !fresh {
		t.Fatal("clean tree should be fresh immediately after synthesis")
	}
	// Uncommitted edit, HEAD unchanged — MUST invalidate (the "not yet committed" bug).
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1-edited\n"), 0o644)
	if _, fresh := Load(ctx, st, dir); fresh {
		t.Fatal("an uncommitted edit must invalidate the overview (dirty-state changed)")
	}
	// Committing it (clean again, new HEAD) must also invalidate.
	git(t, dir, "commit", "-aqm", "edit")
	if _, fresh := Load(ctx, st, dir); fresh {
		t.Fatal("a commit must invalidate the overview")
	}
}

// TestGatherStatesWorkingTreeTruth: the synthesis input LEADS with accurate git
// status, so the model never narrates "uncommitted" on a clean tree.
func TestGatherStatesWorkingTreeTruth(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	clean := gather(ctx, st, dir, snapshot(ctx, dir))
	if !strings.Contains(clean, "CLEAN") {
		t.Errorf("clean tree must report CLEAN in the synthesis input:\n%s", clean)
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644)
	dirty := gather(ctx, st, dir, snapshot(ctx, dir))
	if !strings.Contains(dirty, "DIRTY") || !strings.Contains(dirty, "b.txt") {
		t.Errorf("dirty tree must report DIRTY + the file:\n%s", dirty)
	}
}
