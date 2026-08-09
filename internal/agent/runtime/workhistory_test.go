package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

// TestWorkHistoryGitAnchored: "what did we work on" must report ACTUAL git commits
// (the actor-agnostic truth), not the memcode session log — so work committed by
// any tool/hand shows up.
func TestWorkHistoryGitAnchored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("1\n"), 0o644)
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "feat: alpha thing")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("2\n"), 0o644)
	gitCmd(t, dir, "commit", "-aqm", "fix: beta thing")

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newSess(st, usageProvider{}, dir, "sonnet", permissions.ModeAsk, io.Discard)
	s.sessionID = "sess_test"

	out, _ := s.Intelligence(ctx, "session", "") // recap default → workHistory (git-anchored)
	if !strings.Contains(out, "git commits") {
		t.Errorf("work history must lead with git commits, got:\n%s", out)
	}
	for _, subj := range []string{"alpha thing", "beta thing"} {
		if !strings.Contains(out, subj) {
			t.Errorf("missing real commit %q (git is the truth):\n%s", subj, out)
		}
	}
}
