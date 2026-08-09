package runtime

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/jobs"
	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func newJobSession(t *testing.T) (*Session, context.Context) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAllowAll, io.Discard)
	s.bgCtx = ctx
	return s, ctx
}

func waitRunning(t *testing.T, s *Session, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.RunningJobs() != n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.RunningJobs() != n {
		t.Fatalf("want %d running shells, got %d", n, s.RunningJobs())
	}
}

// The exact case Tim hit: a first attempt fails to start, the second works. The
// working server must be SHELL 1 (the failed attempt holds no slot), so /kill 1
// matches the user's intuition.
func TestFailedAttemptDoesNotHoldShellSlot(t *testing.T) {
	s, ctx := newJobSession(t)

	jobs.Start(s.bgJobs, ctx, "exit 1", s.root) // internal id 1 — fails immediately
	waitRunning(t, s, 0)
	jobs.Start(s.bgJobs, ctx, "sleep 30", s.root) // internal id 2 — the real server
	waitRunning(t, s, 1)

	if got := s.JobsRender(); !strings.Contains(got, "shell 1") || strings.Contains(got, "shell 2") {
		t.Errorf("the running server should be shell 1 (failed attempt holds no slot):\n%s", got)
	}
	if got := s.KillJobArg("1"); !strings.Contains(got, "stopped shell 1") {
		t.Errorf("/kill 1 should stop the running server: %q", got)
	}
	if s.RunningJobs() != 0 {
		t.Errorf("running=%d after kill, want 0", s.RunningJobs())
	}
}

// /kill ergonomics over slots: bare /kill stops the lone shell; an out-of-range slot
// reports what's running.
func TestKillJobArgErgonomics(t *testing.T) {
	s, ctx := newJobSession(t)

	if got := s.KillJobArg(""); !strings.Contains(got, "no running shells") {
		t.Errorf("no shells: %q", got)
	}

	jobs.Start(s.bgJobs, ctx, "sleep 30", s.root)
	jobs.Start(s.bgJobs, ctx, "sleep 30", s.root)
	waitRunning(t, s, 2)

	if got := s.KillJobArg("5"); !strings.Contains(got, "no running shell 5") {
		t.Errorf("out-of-range slot should list running: %q", got)
	}
	if got := s.KillJobArg("1"); !strings.Contains(got, "stopped shell 1") {
		t.Errorf("/kill 1 should stop shell 1: %q", got)
	}
	waitRunning(t, s, 1)
	if got := s.KillJobArg(""); !strings.Contains(got, "stopped shell 1") {
		t.Errorf("bare /kill should stop the lone remaining shell: %q", got)
	}
}
