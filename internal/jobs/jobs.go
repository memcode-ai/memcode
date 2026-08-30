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
	StatusWaiting = "waiting"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusStopped = "stopped" // process gone but never recorded a finish
)

// Job is one background agent session.
type ToolPolicy struct {
	Allowed  []string `json:"allowed,omitempty"`
	Disabled []string `json:"disabled,omitempty"`
}
type ResourceGrant struct {
	IDs []string `json:"ids,omitempty"`
}
type ExecutionBudgets struct {
	MaxSeconds         int `json:"max_seconds,omitempty"`
	MaxToolCalls       int `json:"max_tool_calls,omitempty"`
	MaxDelegationDepth int `json:"max_delegation_depth,omitempty"`
}

type SpawnSpec struct {
	Root, Task, Mode, Tier, SessionID, AgentID, ObjectiveID, SubgoalID, RunID, ParentRunID, PolicyHash, BrowserMode string
	ToolPolicy                                                                                                      ToolPolicy
	ResourceGrant                                                                                                   ResourceGrant
	Budgets                                                                                                         ExecutionBudgets
	ReportBack                                                                                                      bool
}

type Job struct {
	ID         string `json:"id"`
	Task       string `json:"task"`
	Mode       string `json:"mode"`
	Tier       string `json:"tier,omitempty"`        // "strong" → the detached child runs on the frontier tier (agent_frontier)
	ReportBack bool   `json:"report_back,omitempty"` // true → on finish, feed Result back to the calling LLM (agent{background}), not just the user
	Result     string `json:"result,omitempty"`      // the agent's final text (set by Finish) — what gets reported back
	PID        int    `json:"pid"`
	// StartSig is the child process's start-time signature captured at spawn (ps lstart on
	// unix; "" where unavailable). PIDs recycle, so before reporting a job as running — and
	// especially before SIGNALING its pid — the live process's signature must match: a
	// mismatch means the pid now belongs to an unrelated process. Additive field; old metas
	// without it keep the plain liveness check.
	StartSig   string    `json:"start_sig,omitempty"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	// Live readout, heartbeated by the running child (~1s) so frontends can show
	// what a detached agent is doing right now. Additive; absent in old metas.
	Activity          string          `json:"activity,omitempty"`   // latest tool label, e.g. "bash(go test ./...)"
	TokensIn          int64           `json:"tokens_in,omitempty"`  // child session input tokens so far
	TokensOut         int64           `json:"tokens_out,omitempty"` // child session output tokens so far
	HeartbeatAt       time.Time       `json:"heartbeat_at,omitempty"`
	AgentID           string          `json:"agent_id,omitempty"`
	ObjectiveID       string          `json:"objective_id,omitempty"`
	SubgoalID         string          `json:"subgoal_id,omitempty"`
	RunID             string          `json:"run_id,omitempty"`
	ParentRunID       string          `json:"parent_run_id,omitempty"`
	SessionID         string          `json:"session_id,omitempty"`
	PolicyHash        string          `json:"policy_hash,omitempty"`
	ExecutionEnvelope json.RawMessage `json:"execution_envelope,omitempty"`
	// NOTE: this struct deliberately carries no suspension/continuation fields.
	// It used to declare InteractionID/WaitingReason/ContinuationVersion/
	// WaitingAt/ResumedAt, which nothing ever wrote — a third half-built
	// suspend/resume design alongside two others. Durable suspension lives in
	// internal/agent/continuation, once. A detached job child runs with
	// SetNoApprover and cannot ask a human mid-run today; if that changes, wire
	// it to that package rather than re-adding fields here.
}

// processMatches reports whether the job's recorded pid is alive AND still the same process
// it was at spawn (start-time signature match). Falls back to plain liveness when either side
// has no signature (old meta, or a platform without one).
func processMatches(job Job) bool {
	if !processAlive(job.PID) {
		return false
	}
	if job.StartSig == "" {
		return true
	}
	sig, ok := processStartSig(job.PID)
	if !ok {
		return true // can't verify — behave as before rather than orphaning a live job
	}
	return sig == job.StartSig
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
	browserMode := ""
	if chrome {
		browserMode = "ephemeral"
	}
	return SpawnWithSpec(SpawnSpec{Root: root, Task: task, Mode: mode, Tier: tier, SessionID: session, BrowserMode: browserMode, ReportBack: reportBack})
}

func SpawnWithSpec(spec SpawnSpec) (Job, error) {
	root, task, mode, tier, reportBack, session := spec.Root, spec.Task, spec.Mode, spec.Tier, spec.ReportBack, spec.SessionID
	chrome := spec.BrowserMode == "ephemeral"
	existingChrome := spec.BrowserMode == "existing_chrome"
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
	if existingChrome {
		// The child dials the gateway-owned browser broker itself (socket path
		// is well-known, see internal/browser/broker.SocketPath), authenticating
		// the lease request as (AgentID, this job's own id) — a job id is unique
		// per delegate call, so it doubles as the lease's RunID.
		argv = append(argv, "--browser-session", "existing_chrome", "--browser-agent", spec.AgentID, "--browser-run", id)
	}
	if session != "" {
		argv = append(argv, "--session", session) // continue this conversation's session (resume-or-create)
	}
	// ToolPolicy is a REAL restriction on the child, not just recorded metadata:
	// --allow-tools/--deny-tools bind the same SetToolPolicy enforcement an
	// ordinary gateway-bound agent gets from its config. A caller (e.g. a
	// autonomous agent's delegate tool) that hands this spec a narrower toolset
	// than the parent policy allows gets an actually narrower child, not just an
	// audited claim of one.
	if len(spec.ToolPolicy.Allowed) > 0 {
		argv = append(argv, "--allow-tools", strings.Join(spec.ToolPolicy.Allowed, ","))
	}
	if len(spec.ToolPolicy.Disabled) > 0 {
		argv = append(argv, "--deny-tools", strings.Join(spec.ToolPolicy.Disabled, ","))
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
	// Identity, not just a pid: record the child's start-time signature so a later Stop/Get
	// can tell this process apart from an unrelated one that recycled the pid.
	sig, _ := processStartSig(pid)
	// Detach: release the child so it keeps running after we return.
	_ = cmd.Process.Release()

	envelope, _ := json.Marshal(spec)
	job := Job{
		ID:         id,
		Task:       task,
		Mode:       mode,
		Tier:       tier,
		ReportBack: reportBack,
		PID:        pid,
		StartSig:   sig,
		Status:     StatusRunning,
		StartedAt:  time.Now().UTC(),
		AgentID:    spec.AgentID, ObjectiveID: spec.ObjectiveID, SubgoalID: spec.SubgoalID,
		RunID: spec.RunID, ParentRunID: spec.ParentRunID, SessionID: spec.SessionID,
		PolicyHash: spec.PolicyHash, ExecutionEnvelope: envelope,
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

// Heartbeat updates a RUNNING job's live activity/token readout. It re-reads
// the meta immediately before writing and refuses to touch a job that is no
// longer running, so it can never resurrect (or clobber) a terminal record —
// if Finish or Stop landed first, their status/result win and the heartbeat
// is dropped. The child stops heartbeating before it calls Finish, so within
// the child process the two never interleave.
func Heartbeat(root, id, activity string, tokensIn, tokensOut int64) error {
	job, err := load(root, id)
	if err != nil {
		return err
	}
	if job.Status != StatusRunning {
		return nil
	}
	job.Activity = activity
	job.TokensIn = tokensIn
	job.TokensOut = tokensOut
	job.HeartbeatAt = time.Now().UTC()
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
	if !processMatches(job) {
		// Already gone — or the pid was recycled by an UNRELATED process, which must
		// never be signaled. Reconcile the meta to stopped so List doesn't keep
		// reporting it as running (the dead-process reconciliation in List would
		// fix it too, but be honest now).
		return markStopped(root, id)
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
	return markStopped(root, id)
}

// markStopped records the stopped status, RE-LOADING the meta first: the child's own Finish
// may have landed while Stop was signaling/waiting, and that terminal record (status, exit
// code, Result) must win — overwriting it with a bare "stopped" would lose the result
// (the Finish/Stop lost-update race).
func markStopped(root, id string) error {
	job, err := load(root, id)
	if err != nil {
		return err
	}
	if job.Status != StatusRunning {
		return nil // the child already recorded its own outcome — keep it
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
		if job.Status == StatusRunning && !processMatches(job) {
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
	if job.Status == StatusRunning && !processMatches(job) {
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
