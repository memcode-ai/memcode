// Package jobs is memcode's background-job layer — the async extension of the shell
// lanes. A dev server / watcher / `docker compose up` never exits, so it can't run
// through the blocking bash path; it runs here: started detached, output captured to
// a capped ring buffer, status tracked, killable. Doctrine: foreground answers now;
// background jobs maintain temporal state; monitors turn waiting into memory.
//
// Concurrency note: the registry is the runtime's first concurrent subsystem. All
// mutable Job state is guarded by Registry.mu; callers only ever see immutable View
// snapshots. Jobs use a process GROUP (Setpgid) so killing one reaps the whole tree
// (npm → node → …), and they run under a background context so they outlive the turn.
package jobs

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	Running Status = "running"
	Exited  Status = "exited" // finished with exit 0
	Failed  Status = "failed" // finished non-zero
	Killed  Status = "killed" // we killed it (/kill or session end)
)

// View is an immutable snapshot of a job for callers (footer, /jobs, /tail).
type View struct {
	ID      int
	Command string
	Status  Status
	Exit    int
	Started time.Time
	Ended   time.Time
}

type job struct {
	id         int
	command    string
	cwd        string
	started    time.Time
	status     Status
	exit       int
	ended      time.Time
	cmd        *exec.Cmd
	out        stringer
	reportBack bool // promoted foreground command — hand its result back to the model on completion
}

// stringer is the job's output sink read by /tail and report-back. Both the capped ring
// (dev servers) and the promoted-command buffer satisfy it.
type stringer interface{ String() string }

// ShellReport is a finished report-back shell job — a background shell or a
// promoted foreground command — owed back to the model. Drained by the UI poll
// and injected as a new turn.
type ShellReport struct {
	ID      int
	Command string
	Status  Status
	Exit    int
	Output  string
}

// Registry owns the live background jobs for a session.
type Registry struct {
	mu      sync.Mutex
	jobs    map[int]*job
	next    int
	reports []ShellReport // finished promoted commands awaiting hand-back to the model
}

func New() *Registry { return &Registry{jobs: map[int]*job{}} }

// Start launches command (via the platform shell) in cwd as a detached background
// job and returns its View. ctx should be a LONG-LIVED context (session/background)
// — never a turn context, or the job dies when the turn ends. Every started job
// reports back on its own exit (reportBack=true): a finished poller hands its
// result to the model, and a dev server that exits did so unexpectedly — exactly
// when you want to hear about it. Jobs WE kill (/kill, session end) stay silent.
func Start(r *Registry, ctx context.Context, command, cwd string) (View, error) {
	c := shellCommand(command)
	c.Dir = cwd
	setProcessGroup(c) // own process group → kill the tree
	out := &ring{cap: ringCap}
	c.Stdout = out
	c.Stderr = out
	if err := c.Start(); err != nil {
		return View{}, err
	}

	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	return r.track(ctx, c, out, done, command, cwd, time.Now(), true), nil
}

// track registers an already-started command in the registry and watches it for exit.
// `done` must deliver the single c.Wait() result. ctx cancellation kills the group (so a
// job never outlives the session). reportBack marks a promoted foreground command whose
// result is owed back to the model on completion.
func (r *Registry) track(ctx context.Context, c *exec.Cmd, out stringer, done <-chan error, command, cwd string, started time.Time, reportBack bool) View {
	r.mu.Lock()
	r.next++
	j := &job{id: r.next, command: command, cwd: cwd, started: started, status: Running, cmd: c, out: out, reportBack: reportBack}
	r.jobs[j.id] = j
	r.mu.Unlock()

	go func() {
		var werr error
		select {
		case werr = <-done:
			werr = waitErr(werr)
		case <-ctx.Done():
			killGroup(c)
			<-done // reap
			r.finish(j, Killed, 0)
			return
		}
		r.mu.Lock()
		killed := j.status == Killed
		r.mu.Unlock()
		if killed {
			return
		}
		if werr != nil {
			r.finish(j, Failed, exitCode(c))
		} else {
			r.finish(j, Exited, 0)
		}
	}()

	return view(j)
}

// ForegroundOutcome is the result of RunForegroundOrPromote.
type ForegroundOutcome struct {
	Done     bool   // ran to completion within the time budget
	Killed   bool   // turnCtx was cancelled (Esc/Ctrl-C) — the whole group was reaped
	Exit     int    // exit code when Done (-1 if it couldn't start)
	Stdout   string // captured stdout when Done
	Stderr   string // captured stderr when Done
	Promoted *View  // non-nil: still running at the deadline, handed to a background report-back job
}

// RunForegroundOrPromote runs command in its OWN process group and waits up to `timeout`
// for it to finish. Three outcomes:
//   - it finishes → Done, with Exit + Stdout + Stderr (the old blocking bash path);
//   - turnCtx is cancelled first (Esc/Ctrl-C) → Killed, the group reaped;
//   - it's STILL running at the deadline → it is NOT killed. It's promoted to a tracked
//     background job (watched under bgCtx, reporting its result back on completion) and
//     returned via Promoted, so the turn stops blocking on it.
//
// bgCtx MUST be the long-lived session context so a promoted job outlives the turn;
// turnCtx is the per-turn context that carries interrupt.
func RunForegroundOrPromote(r *Registry, bgCtx, turnCtx context.Context, timeout time.Duration, command, cwd string) ForegroundOutcome {
	c := shellCommand(command)
	c.Dir = cwd
	setProcessGroup(c)
	outBuf, errBuf := &SyncBuf{}, &SyncBuf{} // unbounded while foreground — matches the old strings.Builder capture
	c.Stdout = outBuf
	c.Stderr = errBuf
	started := time.Now()
	if err := c.Start(); err != nil {
		return ForegroundOutcome{Done: true, Exit: -1, Stderr: "could not run: " + err.Error()}
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return ForegroundOutcome{Done: true, Exit: exitCode(c), Stdout: outBuf.String(), Stderr: errBuf.String()}
	case <-turnCtx.Done():
		killGroup(c)
		<-done // reap: the whole group is dead, so the pipes close and Wait returns
		return ForegroundOutcome{Killed: true, Exit: -1, Stdout: outBuf.String(), Stderr: errBuf.String()}
	case <-timer.C:
		// Outlived the turn budget — promote instead of killing. Bound memory now that it
		// may run a long time (keep the tail, like a dev server's ring).
		outBuf.Cap(ringCap)
		errBuf.Cap(ringCap)
		v := r.track(bgCtx, c, combinedBuf{outBuf, errBuf}, done, command, cwd, started, true)
		return ForegroundOutcome{Promoted: &v}
	}
}

// DrainReports returns and clears the finished report-back shell jobs (promoted commands),
// each exactly once. Called from the UI poll to hand results back to the model.
func (r *Registry) DrainReports() []ShellReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reports) == 0 {
		return nil
	}
	out := r.reports
	r.reports = nil
	return out
}

// pipeGrace bounds how long Wait may block on open output pipes AFTER the shell
// process itself has exited. Without it, a command that finishes fast but leaves
// any child behind (a `foo &`, gcloud's helper processes) holds the pipe write-end
// open and c.Wait() never returns — the foreground path blocks its whole budget,
// "promotes" a command that already finished, and the report-back waits on the
// straggler instead of the shell. WaitDelay starts at process exit, so a command
// whose pipes close normally is completely unaffected.
const pipeGrace = 2 * time.Second

// shellCommand builds the platform shell invocation (mirrors runtime.shellCmd).
func shellCommand(command string) *exec.Cmd {
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("powershell", "-NoProfile", "-Command", command)
	} else {
		c = exec.Command("sh", "-c", command)
	}
	c.WaitDelay = pipeGrace
	return c
}

// waitErr normalizes a c.Wait() error: ErrWaitDelay means the PROCESS exited
// successfully and only a lingering pipe-holder forced the pipes closed — that is
// success, not failure (Go returns ErrWaitDelay only in place of nil).
func waitErr(err error) error {
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

// RunForeground runs cmd to completion in its OWN process group, killing the whole
// group if ctx is cancelled or times out — so a child (e.g. node under npm) can't
// hold the output pipe open and hang cmd.Wait() after the kill. Use this instead of
// exec.CommandContext + cmd.Run() for any foreground command that may spawn children:
// CommandContext kills only the direct child, which is why an un-grouped `next dev`
// made the agent turn unkillable (Esc/Ctrl-C/timeout couldn't free it). Returns
// ctx.Err() on cancel, else the command's exit error.
func RunForeground(ctx context.Context, cmd *exec.Cmd) error {
	setProcessGroup(cmd)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = pipeGrace // same lingering-pipe-holder guard as shellCommand
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return waitErr(err)
	case <-ctx.Done():
		killGroup(cmd)
		<-done // reap: the whole group is dead, so the pipe closes and Wait returns
		return ctx.Err()
	}
}

func (r *Registry) finish(j *job, st Status, exit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j.status == Killed && st != Killed {
		return // a kill already won the race
	}
	j.status = st
	j.exit = exit
	j.ended = time.Now()
	// A report-back job that ran to completion (not one we killed) owes its result
	// back to the model — queue it for the UI poll to inject as a new turn. That's
	// every background job (Start) and every promoted foreground command.
	if j.reportBack && st != Killed {
		r.reports = append(r.reports, ShellReport{ID: j.id, Command: j.command, Status: st, Exit: exit, Output: j.out.String()})
	}
}

// List returns snapshots of all jobs, running first then most-recent.
func (r *Registry) List() []View {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []View
	for _, j := range r.jobs {
		out = append(out, view(j))
	}
	// running first, then by id desc (most recent first)
	sortViews(out)
	return out
}

// Running returns how many jobs are still running (for the footer "N shells").
func (r *Registry) Running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, j := range r.jobs {
		if j.status == Running {
			n++
		}
	}
	return n
}

// Tail returns the last n lines of a job's captured output.
func (r *Registry) Tail(id, n int) (string, bool) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	r.mu.Unlock()
	if !ok {
		return "", false
	}
	lines := strings.Split(strings.TrimRight(j.out.String(), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), true
}

// Kill terminates a running job's process group. Returns whether a running job was killed.
func (r *Registry) Kill(id int) bool {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if !ok || j.status != Running {
		r.mu.Unlock()
		return false
	}
	j.status = Killed
	j.ended = time.Now()
	c := j.cmd
	r.mu.Unlock()
	killGroup(c)
	return true
}

// KillAll terminates every running job — call on session end so nothing orphans.
func (r *Registry) KillAll() {
	r.mu.Lock()
	var cs []*exec.Cmd
	for _, j := range r.jobs {
		if j.status == Running {
			j.status = Killed
			j.ended = time.Now()
			cs = append(cs, j.cmd)
		}
	}
	r.mu.Unlock()
	for _, c := range cs {
		killGroup(c)
	}
}

func view(j *job) View {
	return View{ID: j.id, Command: j.command, Status: j.status, Exit: j.exit, Started: j.started, Ended: j.ended}
}

func sortViews(vs []View) {
	for i := 1; i < len(vs); i++ {
		for k := i; k > 0 && less(vs[k], vs[k-1]); k-- {
			vs[k], vs[k-1] = vs[k-1], vs[k]
		}
	}
}

// less: running before finished; within a group, higher id (more recent) first.
func less(a, b View) bool {
	ar, br := a.Status == Running, b.Status == Running
	if ar != br {
		return ar
	}
	return a.ID > b.ID
}

func exitCode(c *exec.Cmd) int {
	if c.ProcessState != nil {
		return c.ProcessState.ExitCode()
	}
	return -1
}

const ringCap = 64 * 1024 // keep the last 64KB of a job's output

// ring is a thread-safe, byte-capped tail buffer used as the job's combined
// stdout+stderr sink (both streams write here, so it's the interleaved log tail).
type ring struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.buf = append(r.buf[:0], r.buf[len(r.buf)-r.cap:]...)
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// SyncBuf is a thread-safe output sink: unbounded while a command runs in the foreground
// (so a normal bash call's full output is captured, as the old strings.Builder did), then
// capped to its tail via Cap() if the command is promoted to a long-running background job.
type SyncBuf struct {
	mu  sync.Mutex
	buf []byte
	cap int // 0 = unbounded
}

func (b *SyncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.cap > 0 && len(b.buf) > b.cap {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.cap:]...)
	}
	return len(p), nil
}

func (b *SyncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Cap bounds the buffer to its last n bytes and keeps it that way — called on promotion.
func (b *SyncBuf) Cap(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cap = n
	if n > 0 && len(b.buf) > n {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-n:]...)
	}
}

// combinedBuf presents a promoted job's stdout+stderr as one log tail for /tail and report-back.
type combinedBuf struct{ o, e *SyncBuf }

func (c combinedBuf) String() string {
	so, se := c.o.String(), c.e.String()
	switch {
	case se == "":
		return so
	case so == "":
		return se
	default:
		return so + "\n" + se
	}
}
