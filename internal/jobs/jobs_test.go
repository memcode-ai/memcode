package jobs

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestMetaRoundTripListFinish(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_test", Task: "do a thing", Mode: "auto", PID: os.Getpid(),
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}

	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "job_test" {
		t.Fatalf("List = %+v", list)
	}
	// Our own pid is alive ⇒ still running.
	if list[0].Status != StatusRunning {
		t.Fatalf("status = %q, want running", list[0].Status)
	}

	if err := Finish(root, job.ID, 0, "the agent's final result"); err != nil {
		t.Fatal(err)
	}
	got, err := Get(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.FinishedAt.IsZero() {
		t.Fatalf("after Finish: %+v", got)
	}
	if got.Result != "the agent's final result" {
		t.Errorf("Finish must persist the result for report-back, got %q", got.Result)
	}
}

func TestListReconcilesDeadProcess(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_dead", Task: "x", PID: 2147483646, // not a live pid
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
	list, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Status != StatusStopped {
		t.Fatalf("dead-process job status = %q, want stopped", list[0].Status)
	}
}

// A recycled pid (alive, but with a DIFFERENT start-time signature than the one recorded at
// spawn) must be treated as gone: reported stopped, and — the load-bearing part — never
// signaled by Stop.
func TestPIDRecyclingIdentityCheck(t *testing.T) {
	if _, ok := processStartSig(os.Getpid()); !ok {
		t.Skip("no process start signature on this platform")
	}
	root := t.TempDir()
	job := Job{ID: "job_recycled", Task: "x", PID: os.Getpid(), // alive, but…
		StartSig: "Mon Jan  1 00:00:00 1990", // …recorded for a different incarnation
		Status:   StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
	got, err := Get(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped {
		t.Fatalf("recycled-pid job status = %q, want stopped", got.Status)
	}
	// Stop must reconcile without signaling the unrelated (here: our own) process.
	if err := Stop(root, job.ID); err != nil {
		t.Fatal(err)
	}
	after, err := load(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusStopped {
		t.Fatalf("after Stop: status = %q, want stopped", after.Status)
	}
	// And a MATCHING signature keeps the job running.
	sig, _ := processStartSig(os.Getpid())
	job2 := Job{ID: "job_live", Task: "x", PID: os.Getpid(), StartSig: sig,
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job2.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job2); err != nil {
		t.Fatal(err)
	}
	if got, err := Get(root, job2.ID); err != nil || got.Status != StatusRunning {
		t.Fatalf("matching-signature job: %+v, %v", got, err)
	}
}

// Stop must not clobber a terminal record the child already wrote (the Finish/Stop
// lost-update race): after the process is gone, a Finish that landed first wins.
func TestStopKeepsChildRecordedFinish(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_finished", Task: "x", PID: 2147483646, // not a live pid
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
	if err := Finish(root, job.ID, 0, "final result"); err != nil {
		t.Fatal(err)
	}
	// Status is done now, so Stop refuses politely — the guard we're testing is
	// markStopped's re-load; drive it directly the way the post-signal path does.
	if err := markStopped(root, job.ID); err != nil {
		t.Fatal(err)
	}
	got, err := load(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || got.Result != "final result" {
		t.Fatalf("child's Finish record was clobbered: %+v", got)
	}
}

func TestWriterLockSerializes(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	release1, err := AcquireWriter(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	// A second acquire must block while the first holds the lock.
	got := make(chan func(), 1)
	go func() {
		r2, err := AcquireWriter(ctx, root)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		got <- r2
	}()

	select {
	case <-got:
		t.Fatal("second AcquireWriter returned while the lock was held")
	case <-time.After(700 * time.Millisecond):
		// expected: still blocked
	}

	release1()
	select {
	case r2 := <-got:
		r2()
	case <-time.After(3 * time.Second):
		t.Fatal("second AcquireWriter did not proceed after release")
	}
}

func TestStaleLockReclaimed(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a lock owned by a dead pid.
	if err := os.WriteFile(lockPath(root), []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		release, err := AcquireWriter(context.Background(), root)
		if err != nil {
			t.Errorf("acquire over stale lock: %v", err)
		} else {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireWriter did not reclaim a stale lock")
	}
}

func TestIsTestBinary(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/go-build123/b507/runtime.test", true},
		{"/tmp/go-build123/b507/vxui.test.exe", true},
		{"/usr/local/bin/memcode", false},
		{"/Users/x/cli/dev/memcode", false},
	}
	for _, c := range cases {
		if got := isTestBinary(c.path); got != c.want {
			t.Errorf("isTestBinary(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestSpawnFromTestBinaryChildExitsImmediately is the fork-bomb regression: Spawn
// called from a test binary (os.Executable() = jobs.test) must NOT re-exec the
// suite — the child it starts has to exit on its own almost immediately. Before
// the isTestBinary guard, the child ran the caller's entire test suite and every
// Spawn-reaching test in it spawned another detached child, exponentially.
func TestSpawnFromTestBinaryChildExitsImmediately(t *testing.T) {
	root := t.TempDir()
	job, err := Spawn(root, "regression: do nothing", "auto", "", false, false, "")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(job.PID) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(job.PID, syscall.SIGKILL)
			t.Fatalf("spawned child (pid %d) still alive after 10s — test-binary re-exec guard failed", job.PID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStopKillsRunningProcessAndRecordsStopped starts a real long-running child
// (sleep), records it as a running job, then Stop must signal it and mark it
// stopped. This is the safety valve for a runaway detached agent.
func TestStopKillsRunningProcessAndRecordsStopped(t *testing.T) {
	root := t.TempDir()

	// Spawn a real child we can signal — `sleep 30` won't exit on its own.
	cmd := exec.CommandContext(context.Background(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("can't start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { // cleanup if the test bails early
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	job := Job{ID: "job_stopme", Task: "runaway", Mode: "auto", PID: pid,
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}

	if err := Stop(root, job.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	got, err := Get(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
	if got.FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set after Stop")
	}
	if processAlive(pid) {
		t.Fatalf("pid %d still alive after Stop", pid)
	}
}

// TestStopRejectsNonRunningJob verifies Stop refuses a job that already finished —
// you can't stop something that's not running.
func TestStopRejectsNonRunningJob(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_done", Task: "x", PID: 2147483646,
		Status: StatusDone, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
	if err := Stop(root, job.ID); err == nil {
		t.Fatal("Stop on a done job should error")
	}
}

// TestStopReconcilesAlreadyGoneProcess verifies Stop marks a job as stopped even
// when its process already died (meta says running, but PID is gone — e.g. the
// child crashed without recording a finish).
func TestStopReconcilesAlreadyGoneProcess(t *testing.T) {
	root := t.TempDir()
	job := Job{ID: "job_gone", Task: "x", PID: 2147483646, // not a live pid
		Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := os.MkdirAll(jobDir(root, job.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(root, job); err != nil {
		t.Fatal(err)
	}
	if err := Stop(root, job.ID); err != nil {
		t.Fatalf("Stop on already-gone job: %v", err)
	}
	got, err := Get(root, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
}

// TestGetIsRootScoped documents the invariant behind the gateway reply-polling
// fix: a job's bookkeeping lives under the root it was spawned into, so a reader
// (waitForJob) must poll the SAME root. Found under its own root, absent under
// another — polling the wrong root would report "Lost track of the job" for any
// task that ran in a non-default project.
func TestGetIsRootScoped(t *testing.T) {
	spawnRoot := t.TempDir()
	otherRoot := t.TempDir()
	job, err := Spawn(spawnRoot, "task", "auto", "", false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Get(spawnRoot, job.ID); err != nil {
		t.Errorf("job must be found under its spawn root: %v", err)
	}
	if _, err := Get(otherRoot, job.ID); err == nil {
		t.Error("job must NOT resolve under a different root; the poller has to use the spawn root")
	}
}
