package acceptance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/memcode-ai/memcode/internal/agent/edit"
	"github.com/memcode-ai/memcode/internal/events"
	"github.com/memcode-ai/memcode/internal/store"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		files []fileState
		want  string
	}{
		{"committed intact → accepted",
			[]fileState{{postHash: "h2", curHash: "h2", committedSince: true}}, Accepted},
		{"reverted (clean, content differs, not committed) → rejected",
			[]fileState{{postHash: "h2", curHash: "h1", committedSince: false}}, Rejected},
		{"file gone → rejected",
			[]fileState{{postHash: "h2", curHash: ""}}, Rejected},
		{"committed after human edit → corrected",
			[]fileState{{postHash: "h2", curHash: "h3", committedSince: true}}, Corrected},
		{"still dirty, further edited → corrected",
			[]fileState{{postHash: "h2", curHash: "h3", dirty: true}}, Corrected},
		{"still dirty, unchanged → unknown",
			[]fileState{{postHash: "h2", curHash: "h2", dirty: true}}, Unknown},
		{"mixed: one accepted one reverted → rejected wins (tie)",
			[]fileState{{postHash: "a", curHash: "a", committedSince: true}, {postHash: "b", curHash: "x"}}, Rejected},
	}
	for _, c := range cases {
		got, _, _ := classify(c.files)
		if got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func head(t *testing.T, dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out[:len(out)-1])
}

func TestReconcileAcceptedAndRejected(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	write(t, dir, "a.go", "v1\n")
	write(t, dir, "b.go", "b1\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "base")
	base := head(t, dir)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Session A edits a.go; Session B edits b.go.
	write(t, dir, "a.go", "v2\n")
	aHash, _ := edit.Hash(dir, "a.go")
	emitSession(ctx, t, st, "sessA", base, "a.go", aHash)

	write(t, dir, "b.go", "b2\n")
	bHash, _ := edit.Hash(dir, "b.go")
	emitSession(ctx, t, st, "sessB", base, "b.go", bHash)

	// Accept A: commit just a.go. Reject B: discard the change.
	git(t, dir, "add", "a.go")
	git(t, dir, "commit", "-q", "-m", "keep a")
	git(t, dir, "checkout", "--", "b.go")

	results, err := Reconcile(ctx, st, dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range results {
		got[r.SessionID] = r.Outcome
	}
	if got["sessA"] != Accepted {
		t.Errorf("sessA outcome = %q, want accepted", got["sessA"])
	}
	if got["sessB"] != Rejected {
		t.Errorf("sessB outcome = %q, want rejected", got["sessB"])
	}

	// Re-running must not re-emit (already reconciled).
	again, _ := Reconcile(ctx, st, dir)
	if len(again) != 0 {
		t.Errorf("second Reconcile re-emitted %d outcomes, want 0", len(again))
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func emitSession(ctx context.Context, t *testing.T, st store.Store, id, base, path, hash string) {
	t.Helper()
	mustEmit(ctx, t, st, events.KindAgentSessionStarted, map[string]any{"session_id": id, "head_sha": base})
	mustEmit(ctx, t, st, events.KindFileEdited, map[string]any{"session_id": id, "path": path, "hash": hash})
	mustEmit(ctx, t, st, events.KindAgentSessionFinished, map[string]any{"session_id": id})
}

func mustEmit(ctx context.Context, t *testing.T, st store.Store, k events.Kind, p map[string]any) {
	t.Helper()
	if _, err := events.Append(ctx, st, k, "test", p); err != nil {
		t.Fatal(err)
	}
}
