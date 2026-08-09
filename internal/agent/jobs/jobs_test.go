package jobs

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The lockup regression: a foreground command whose CHILD holds stdout open (sh
// backgrounds a sleep that inherits the pipe, then waits). exec.CommandContext would
// kill only sh, leaving the sleep to hold the pipe and hang Wait — making the turn
// unkillable. RunForeground kills the whole process GROUP, so cancel returns promptly.
func TestRunForegroundReapsChildHoldingPipe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command("sh", "-c", "sleep 30 & echo started; wait")
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out

	done := make(chan error, 1)
	go func() { done <- RunForeground(ctx, cmd) }()
	time.Sleep(150 * time.Millisecond) // let it start the child
	cancel()

	select {
	case <-done: // returned promptly after cancel — the group (incl. the sleep) was reaped
	case <-time.After(5 * time.Second):
		t.Fatal("RunForeground hung after cancel — the child's pipe wasn't reaped (the unkillable-turn bug)")
	}
}

// A command that finishes within the budget returns Done with its output — the normal
// (unchanged) blocking bash path.
func TestForegroundOrPromoteFinishesInTime(t *testing.T) {
	r := New()
	bg := context.Background()
	oc := RunForegroundOrPromote(r, bg, bg, 3*time.Second, "echo hello; echo oops 1>&2", t.TempDir())
	if oc.Promoted != nil {
		t.Fatalf("fast command was promoted; want Done")
	}
	if !oc.Done || oc.Exit != 0 {
		t.Fatalf("want Done exit 0, got %+v", oc)
	}
	if !strings.Contains(oc.Stdout, "hello") || !strings.Contains(oc.Stderr, "oops") {
		t.Fatalf("streams not captured: stdout=%q stderr=%q", oc.Stdout, oc.Stderr)
	}
	if len(r.List()) != 0 {
		t.Fatalf("a finished foreground command should NOT register a job; got %+v", r.List())
	}
}

// A command that outruns the budget is NOT killed — it's promoted to a tracked background
// job that keeps running and reports its result back on completion.
func TestForegroundOrPromoteAtDeadline(t *testing.T) {
	r := New()
	bg := context.Background()
	oc := RunForegroundOrPromote(r, bg, bg, 100*time.Millisecond, "sleep 0.5; echo done-late", t.TempDir())
	if oc.Promoted == nil {
		t.Fatalf("slow command was not promoted; got %+v", oc)
	}
	// The promoted job keeps running, then finishes on its own.
	v := waitStatus(t, r, oc.Promoted.ID, Exited)
	out, _ := r.Tail(v.ID, 0)
	if !strings.Contains(out, "done-late") {
		t.Fatalf("promoted job's later output not captured: %q", out)
	}
	// On completion it owes a report back to the model — exactly once.
	reps := r.DrainReports()
	if len(reps) != 1 || reps[0].ID != v.ID || !strings.Contains(reps[0].Output, "done-late") {
		t.Fatalf("want one report-back for the promoted job, got %+v", reps)
	}
	if got := r.DrainReports(); got != nil {
		t.Fatalf("report should drain once, got %+v on second drain", got)
	}
}

// Cancelling the turn ctx before the deadline kills the command (interrupt), and it is
// NOT promoted or reported back.
func TestForegroundOrPromoteInterrupt(t *testing.T) {
	r := New()
	turn, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	oc := RunForegroundOrPromote(r, context.Background(), turn, 5*time.Second, "sleep 30", t.TempDir())
	if oc.Promoted != nil || !oc.Killed {
		t.Fatalf("interrupted command should be Killed, not promoted; got %+v", oc)
	}
	if got := r.DrainReports(); got != nil {
		t.Fatalf("an interrupted command owes no report-back, got %+v", got)
	}
}

func waitStatus(t *testing.T, r *Registry, id int, want Status) View {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range r.List() {
			if v.ID == id && v.Status == want {
				return v
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %s; state=%+v", id, want, r.List())
	return View{}
}

// A finite job runs to completion, its output is captured, and exit is recorded.
func TestFiniteJobCapturesAndExits(t *testing.T) {
	r := New()
	v, err := Start(r, context.Background(), "echo capture-me", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, v.ID, Exited)
	out, ok := r.Tail(v.ID, 0)
	if !ok || !strings.Contains(out, "capture-me") {
		t.Errorf("output not captured: ok=%v out=%q", ok, out)
	}
}

// A failing job records Failed + the exit code; stderr is captured too.
func TestFailingJobRecordsExit(t *testing.T) {
	r := New()
	v, _ := Start(r, context.Background(), "echo oops 1>&2; exit 3", t.TempDir())
	got := waitStatus(t, r, v.ID, Failed)
	if got.Exit != 3 {
		t.Errorf("exit = %d, want 3", got.Exit)
	}
	if out, _ := r.Tail(v.ID, 0); !strings.Contains(out, "oops") {
		t.Errorf("stderr not captured: %q", out)
	}
}

// A long-running job stays Running, counts toward Running(), and Kill stops it.
func TestKillStopsRunningJob(t *testing.T) {
	r := New()
	v, _ := Start(r, context.Background(), "while true; do sleep 0.05; done", t.TempDir())
	// Give it a moment to be observed Running.
	time.Sleep(50 * time.Millisecond)
	if r.Running() != 1 {
		t.Fatalf("Running() = %d, want 1", r.Running())
	}
	if !r.Kill(v.ID) {
		t.Fatal("Kill should report it killed a running job")
	}
	waitStatus(t, r, v.ID, Killed)
	if r.Running() != 0 {
		t.Errorf("Running() = %d after kill, want 0", r.Running())
	}
}

// Cancelling the context (session end) kills the job — it must not outlive the session.
func TestContextCancelKillsJob(t *testing.T) {
	r := New()
	ctx, cancel := context.WithCancel(context.Background())
	v, _ := Start(r, ctx, "while true; do sleep 0.05; done", t.TempDir())
	time.Sleep(50 * time.Millisecond)
	cancel()
	waitStatus(t, r, v.ID, Killed)
}

// KillAll reaps everything (session-end cleanup).
func TestKillAll(t *testing.T) {
	r := New()
	for i := 0; i < 3; i++ {
		Start(r, context.Background(), "while true; do sleep 0.05; done", t.TempDir())
	}
	time.Sleep(50 * time.Millisecond)
	if r.Running() != 3 {
		t.Fatalf("Running() = %d, want 3", r.Running())
	}
	r.KillAll()
	deadline := time.Now().Add(2 * time.Second)
	for r.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if r.Running() != 0 {
		t.Errorf("Running() = %d after KillAll, want 0", r.Running())
	}
}

// A background job (bash background:true, `$ cmd &`) reports back on its OWN exit —
// the "poller finished and nobody was told" gap. Exactly once, output included.
func TestStartReportsBackOnExit(t *testing.T) {
	r := New()
	v, err := Start(r, context.Background(), "echo poll-done", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, v.ID, Exited)
	reps := r.DrainReports()
	if len(reps) != 1 || reps[0].ID != v.ID || !strings.Contains(reps[0].Output, "poll-done") {
		t.Fatalf("want one report-back for the finished background job, got %+v", reps)
	}
	if got := r.DrainReports(); got != nil {
		t.Fatalf("report should drain once, got %+v", got)
	}
}

// A failing background job reports back too (a dev server that exits did so
// unexpectedly). One WE kill stays silent — /kill and session end are not news.
func TestStartReportBackFailureAndKillSilence(t *testing.T) {
	r := New()
	v, err := Start(r, context.Background(), "echo boom; exit 3", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, r, v.ID, Failed)
	reps := r.DrainReports()
	if len(reps) != 1 || reps[0].Status != Failed || reps[0].Exit != 3 {
		t.Fatalf("want one FAILED report-back, got %+v", reps)
	}

	k, err := Start(r, context.Background(), "while true; do sleep 0.05; done", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	r.Kill(k.ID)
	waitStatus(t, r, k.ID, Killed)
	if got := r.DrainReports(); got != nil {
		t.Fatalf("a killed job owes no report-back, got %+v", got)
	}
}
