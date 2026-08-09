package runtime

import (
	"context"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/memcode-ai/memcode/internal/agent/permissions"
	"github.com/memcode-ai/memcode/internal/store"
)

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	l.Close()
	return p
}

func respondsWithin(url string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func deadWithin(url string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err != nil {
			return true
		} else {
			resp.Body.Close()
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestBackgroundServerLifecycle is the runtime-level dogfood of the dev-server
// workflow: a `$ … &` server runs in the BACKGROUND (the turn doesn't block),
// actually serves its port, and is fully reaped on /kill (process group). This is
// everything the TUI dogfood checks except the literal footer render + Esc keypress.
func TestBackgroundServerLifecycle(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s := newSess(st, &scriptProvider{}, root, "sonnet", permissions.ModeAsk, io.Discard)
	s.SetApprover(func(_ context.Context, _ ApprovalRequest) ApprovalDecision {
		return ApprovalDecision{Allow: true}
	})
	chat := s.StartChat(ctx)
	defer s.EndChat(ctx)

	port := freePort(t)
	url := "http://127.0.0.1:" + port

	// `$ … &` must NOT block the turn — Submit returns and a job is running.
	s.Submit(ctx, chat, "$ python3 -u -m http.server "+port+" &")
	if s.RunningJobs() != 1 {
		t.Fatalf("expected 1 running background job, got %d", s.RunningJobs())
	}
	if !strings.Contains(s.JobsRender(), "http.server") {
		t.Errorf("/jobs should list the server, got:\n%s", s.JobsRender())
	}

	// The server actually serves in the background.
	if !respondsWithin(url, 4*time.Second) {
		t.Fatal("background server never responded — did the turn block, or did it not start?")
	}

	// /kill (shell 1) reaps the whole tree → the port stops responding, count drops.
	if out := s.KillJobArg("1"); !strings.Contains(out, "stopped shell 1") {
		t.Errorf("KillJobArg: %q", out)
	}
	if !deadWithin(url, 4*time.Second) {
		t.Fatal("server still responding after kill — process group was NOT reaped (the lockup-class bug)")
	}
	if s.RunningJobs() != 0 {
		t.Errorf("RunningJobs = %d after kill, want 0", s.RunningJobs())
	}
}
