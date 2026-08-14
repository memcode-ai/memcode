// Package jobs runs and tracks detached background agent sessions. A job is a
// child `memcode` process whose output streams to a log file under
// .memcode/jobs/<id>/; its metadata (task, pid, status) lives beside the log.
// Background agents are writers, so they coordinate through a single repo-wide
// writer lock — the "one serialized writer" half of memcode's concurrency model:
// any number of read-only explorers may run at once, but mutating jobs run one
// at a time, queueing on the lock rather than clobbering each other's edits.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/memcode-ai/memcode/internal/atomicfile"
)

// Status values for a job.
const (
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusStopped = "stopped" // process gone but never recorded a finish
)

// Job is one background agent session.
type Job struct {
	ID         string    `json:"id"`
	Task       string    `json:"task"`
	Mode       string    `json:"mode"`
	Tier       string    `json:"tier,omitempty"`        // "strong" → the detached child runs on the frontier tier (agent_frontier)
	ReportBack bool      `json:"report_back,omitempty"` // true → on finish, feed Result back to the calling LLM (agent{background}), not just the user
	Result     string    `json:"result,omitempty"`      // the agent's final text (set by Finish) — what gets reported back
	PID        int       `json:"pid"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// dir returns the jobs directory for a project root.
func dir(root string) string { return filepath.Join(root, ".memcode", "jobs") }

func jobDir(root, id string) string   { return filepath.Join(dir(root), id) }
func metaPath(root, id string) string { return filepath.Join(jobDir(root, id), "meta.json") }

// LogPath returns the path to a job's log file.
func LogPath(root, id string) string { return filepath.Join(jobDir(root, id), "log") }

// Spawn launches a detached `memcode run <task>` child, redirecting its output
// to the job log, and records the job as running. The child is invoked with
// --job <id> so it acquires the writer lock and records its own completion.
// When chrome is true, --chrome is forwarded so backgrounded browser jobs keep
// the capability (Chrome always launches with a visible window).
func Spawn(root, task, mode, tier string, chrome, reportBack bool, session string) (Job, error) {
	self, err := os.Executable()
	if err != nil {
		return Job{}, fmt.Errorf("locating memcode binary: %w", err)
	}
	id := newID()
	if err := os.MkdirAll(jobDir(root, id), 0o755); err != nil {
		return Job{}, err
	}
	logf, err := os.Create(LogPath(root, id))
	if err != nil {
		return Job{}, err
	}
	defer logf.Close()

	argv := []string{"run", task, "--" + mode, "--job", id}
	if tier != "" {
		argv = append(argv, "--tier", tier) // the child force-escalates when tier == "strong"
	}
	if reportBack {
		argv = append(argv, "--report-back") // the child persists its final text for report-back
	}
	if chrome {
		argv = append(argv, "--chrome")
	}
	if session != "" {
		argv = append(argv, "--session", session) // continue this conversation's session (resume-or-create)
	}
	if isTestBinary(self) {
		// Under `go test`, os.Executable() is the package's TEST binary, not memcode.
		// Re-execing it as `agent …` runs the caller's whole test suite again: the
		// leading positional arg stops flag parsing, so --auto/--job never error, and
		// that suite's own Spawn-reaching tests each spawn another detached child — an
		// exponential fork bomb that outlives the original `go test` run. Hand the
		// child flags that run zero tests instead: callers still exercise the real
		// Spawn bookkeeping (job dir, log, meta, PID) and the child exits immediately.
		argv = []string{"-test.run", "^$"}
	}
	cmd := exec.Command(self, argv...)
	cmd.Dir = root
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return Job{}, fmt.Errorf("starting job: %w", err)
	}
	pid := cmd.Process.Pid // capture before Release (which zeroes it to -1)
	// Detach: release the child so it keeps running after we return.
	_ = cmd.Process.Release()

	job := Job{
		ID:         id,
		Task:       task,
		Mode:       mode,
		Tier:       tier,
		ReportBack: reportBack,
		PID:        pid,
		Status:     StatusRunning,
		StartedAt:  time.Now().UTC(),
	}
	if err := writeMeta(root, job); err != nil {
		return Job{}, err
	}
	return job, nil
}

// isTestBinary reports whether path is a `go test`-built binary (pkg.test, or
// pkg.test.exe on Windows), so Spawn never re-execs a test suite as an agent.
func isTestBinary(path string) bool {
	base := strings.TrimSuffix(filepath.Base(path), ".exe")
	return strings.HasSuffix(base, ".test")
}

// Finish records a job's terminal status (called by the child when it exits). result is the
// agent's final text, persisted so a report-back job can feed it to the calling LLM.
func Finish(root, id string, exitCode int, result string) error {
	job, err := load(root, id)
	if err != nil {
		return err
	}
	job.ExitCode = exitCode
	job.Result = result
	job.FinishedAt = time.Now().UTC()
	if exitCode == 0 {
		job.Status = StatusDone
	} else {
		job.Status = StatusFailed
	}
	return writeMeta(root, job)
}

// Stop terminates a running job: signals its process (SIGTERM, then SIGKILL if it
// doesn't exit), and records the job as stopped. Returns an error if the job isn't
// found or isn't running. This is the safety valve for a runaway detached agent —
// the TUI/CLI equivalent of /kill for in-session shells.
func Stop(root, id string) error {
	job, err := load(root, id)
	if err != nil {
		return fmt.Errorf("no job %s: %w", id, err)
	}
	if job.Status != StatusRunning {
		return fmt.Errorf("job %s is not running (status: %s)", id, job.Status)
	}
	if !processAlive(job.PID) {
		// Already gone — reconcile the meta to stopped so List doesn't keep
		// reporting it as running (the dead-process reconciliation in List would
		// fix it too, but be honest now).
		job.Status = StatusStopped
		job.FinishedAt = time.Now().UTC()
		return writeMeta(root, job)
	}
	p, err := os.FindProcess(job.PID)
	if err != nil {
		return fmt.Errorf("finding process %d: %w", job.PID, err)
	}
	// SIGTERM first — give the child a moment to clean up (flush logs, release the
	// writer lock). If it doesn't exit, escalate to SIGKILL.
	_ = p.Signal(syscall.SIGTERM)
	if !waitForExit(job.PID, 3*time.Second) {
		_ = p.Signal(syscall.SIGKILL)
		_ = waitForExit(job.PID, 2*time.Second)
	}
	job.Status = StatusStopped
	job.FinishedAt = time.Now().UTC()
	return writeMeta(root, job)
}

// waitForExit polls processAlive every 100ms up to the timeout, returning true if
// the process exited in time. Best-effort — a process that ignores SIGTERM and
// catches SIGKILL (zombie/uninterruptible) will time out and we report it honestly.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// List returns all jobs, newest first, with live status reconciled (a job
// recorded as running whose process is gone is reported as stopped).
func List(root string) ([]Job, error) {
	entries, err := os.ReadDir(dir(root))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		job, err := load(root, e.Name())
		if err != nil {
			continue // skip malformed
		}
		if job.Status == StatusRunning && !processAlive(job.PID) {
			job.Status = StatusStopped
		}
		out = append(out, job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Get returns one job with live status reconciled.
func Get(root, id string) (Job, error) {
	job, err := load(root, id)
	if err != nil {
		return Job{}, err
	}
	if job.Status == StatusRunning && !processAlive(job.PID) {
		job.Status = StatusStopped
	}
	return job, nil
}

func load(root, id string) (Job, error) {
	data, err := os.ReadFile(metaPath(root, id))
	if err != nil {
		return Job{}, err
	}
	var job Job
	if err := json.Unmarshal(data, &job); err != nil {
		return Job{}, err
	}
	return job, nil
}

func writeMeta(root string, job Job) error {
	data, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	// Atomic: a crash mid-write must not truncate meta.json — load rejects it and List then
	// silently drops a still-running detached job (an orphan with no record).
	return atomicfile.WriteFile(metaPath(root, job.ID), append(data, '\n'), 0o644)
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}

// --- writer lock (one serialized writer) ---

func lockPath(root string) string { return filepath.Join(dir(root), ".writer.lock") }

// AcquireWriter blocks until this process holds the repo-wide writer lock, then
// returns a release func. It coordinates background writer jobs so only one
// mutates the tree at a time; a stale lock (owner process gone) is reclaimed.
// ctx cancellation aborts the wait.
func AcquireWriter(ctx context.Context, root string) (release func(), err error) {
	if err := os.MkdirAll(dir(root), 0o755); err != nil {
		return nil, err
	}
	path := lockPath(root)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		// Held — reclaim if the owner is gone, else wait and retry. The reclaim must be
		// ATOMIC: two waiters both seeing a dead owner and both os.Remove(path) would let the
		// second delete the FIRST's freshly-created lock, yielding two live writers. Instead,
		// rename the stale lock aside — os.Rename is atomic, so exactly one waiter wins and is
		// the only one allowed to drop it; the loser retries. If the moved file turns out to
		// belong to a LIVE owner (someone recreated it between our read and our rename), put it
		// back and wait.
		if owner, ok := readPID(path); ok && !processAlive(owner) {
			stale := fmt.Sprintf("%s.reclaim-%d-%s", path, os.Getpid(), randHex())
			if os.Rename(path, stale) != nil {
				continue // lost the reclaim race — retry
			}
			if p, ok := readPID(stale); ok && processAlive(p) {
				_ = os.Rename(stale, path) // it was actually live (recreated) — restore it
			} else {
				_ = os.Remove(stale) // genuinely stale — dropped; retry to O_EXCL-create
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// randHex returns a short random suffix so each waiter's reclaim rename targets a unique path.
func randHex() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func readPID(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(trimSpace(data)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}
