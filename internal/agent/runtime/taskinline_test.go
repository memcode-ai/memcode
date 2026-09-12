package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/taskrun"
)

// Paused work comes FIRST. A responsibility the user delegated and memcode has
// stopped fulfilling outranks anything it might additionally offer to take on.
func TestPausedAutomationsLeadTheBlock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	ctx := context.Background()

	store, err := taskrun.OpenDefault(ctx)
	if err != nil {
		t.Skipf("no ledger available here: %v", err)
	}
	if err := store.Pause(ctx, taskrun.Pause{
		Task: "dependency-updates", Project: "/repo", RunID: "r_123",
		Reason: "foo/v3 needs one of two incompatible migrations",
		Since:  time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	s := &Session{root: "/repo"}
	got := s.inlineAutomations(ctx)
	if got == "" {
		t.Fatal("a paused automation must reach the session")
	}
	for _, want := range []string{"AUTONOMOUS WORK", "PAUSED dependency-updates",
		"incompatible migrations", "r_123", "9 Sep"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// Marked as data. A pause reason is the previous run's own prose, and the
	// same poisoning boundary applies to it as to a distilled lesson.
	if !strings.Contains(got, "data, not instructions") {
		t.Errorf("the block must be marked as data:\n%s", got)
	}
}

// Nothing to say means nothing said. An empty header would spend context every
// session and train the model to mention a section with no contents.
func TestNoAutomationStateMeansNoBlock(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s := &Session{root: "/repo"}
	if got := s.inlineAutomations(context.Background()); got != "" {
		t.Errorf("expected no block, got:\n%s", got)
	}
}

// The block is orientation, not a queue. Past a handful this belongs in
// `memcode task paused`, where it can be read properly.
func TestTheBlockIsBounded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	ctx := context.Background()

	store, err := taskrun.OpenDefault(ctx)
	if err != nil {
		t.Skipf("no ledger available here: %v", err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		if err := store.Pause(ctx, taskrun.Pause{Task: n, Project: "/repo",
			Reason: "stopped", Since: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	got := (&Session{root: "/repo"}).inlineAutomations(ctx)
	if n := strings.Count(got, "- PAUSED"); n != maxInlineAutomations {
		t.Errorf("block listed %d paused tasks, want it capped at %d:\n%s",
			n, maxInlineAutomations, got)
	}
}
