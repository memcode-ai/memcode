package jobs

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A shell that exits fast but leaves a child holding the output pipe must report
// "finished" promptly (bounded by pipeGrace), NOT block its whole foreground budget
// and get falsely promoted. This was the "background shell waits the full timeout
// instead of reporting when it finishes" bug: c.Wait() returns only when the pipes
// close, and a lingering child (`foo &`, gcloud helpers) keeps them open.
func TestLingeringChildDoesNotBlockWait(t *testing.T) {
	r := New()
	start := time.Now()
	oc := RunForegroundOrPromote(r, context.Background(), context.Background(),
		15*time.Second, "sleep 30 & echo done", t.TempDir())
	elapsed := time.Since(start)
	if !oc.Done || oc.Promoted != nil {
		t.Fatalf("fast-exit shell must finish, not promote (done=%v promoted=%v)", oc.Done, oc.Promoted != nil)
	}
	if elapsed > pipeGrace+2*time.Second {
		t.Fatalf("Wait blocked %v — should return within pipeGrace of process exit", elapsed)
	}
	if oc.Exit != 0 || !strings.Contains(oc.Stdout, "done") {
		t.Fatalf("output lost to the lingering child: exit=%d stdout=%q", oc.Exit, oc.Stdout)
	}
}

// Same guard on the detached path: a background job whose shell exits (child
// lingering) must finish Exited and queue its report promptly — the pipe-forced
// ErrWaitDelay is success, not Failed.
func TestLingeringChildBackgroundJobReports(t *testing.T) {
	r := New()
	if _, err := Start(r, context.Background(), "sleep 30 & echo done", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(pipeGrace + 5*time.Second)
	for time.Now().Before(deadline) {
		if reports := r.DrainReports(); len(reports) > 0 {
			rep := reports[0]
			if rep.Status != Exited || rep.Exit != 0 {
				t.Fatalf("lingering-child job must be Exited/0, got %s/%d", rep.Status, rep.Exit)
			}
			if !strings.Contains(rep.Output, "done") {
				t.Fatalf("report lost the job's output: %q", rep.Output)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("no report within pipeGrace — job still blocked on the lingering child's pipe")
}
