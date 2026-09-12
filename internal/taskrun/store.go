// Package taskrun is the durable ledger of autonomous task executions.
//
// A Run is IMMUTABLE HISTORY. It snapshots everything that decided what an
// execution meant — the definition's revision and full text, the resolved
// project, the trigger occurrence, the expanded authority, the chosen runtime,
// the limits — at the moment it is created. Nothing re-reads the YAML mid-run,
// so editing a task file while it executes cannot retroactively change what
// that run was authorized to do or why it did it.
//
// It lives in its own database rather than the gateway's for two reasons:
// `memcode task run` has to work on a machine where the daemon has never
// started (gateway state.OpenShared refuses when there is no gateway.db), and
// the run ledger is an audit trail that must outlive the gateway's operational
// state, which is pruned.
package taskrun

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// State is where a run is in its lifecycle. Terminal state is always Done; what
// actually happened is the Outcome.
type State string

const (
	// StatePending: the row exists and owns its occurrence, but no process has
	// claimed the work yet. Creating the row before doing anything is what makes
	// at-least-once dispatch safe — the unique occurrence index rejects a second
	// row before a second execution can start.
	StatePending State = "pending"
	// StateRunning: a live process holds it, identified by host and pid and
	// proven by a heartbeat.
	StateRunning State = "running"
	// StateDone: terminal. Read Outcome.
	StateDone State = "done"
)

// Outcome is what a finished run actually achieved. "The agent stopped" is
// never by itself "the task succeeded", which is the failure mode this
// distinction exists to prevent.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	// OutcomeNoChange is a successful run that found nothing to do. Worth its own
	// value: a weekly upgrade task reporting no_change is healthy, and collapsing
	// it into success hides whether the task is still finding anything.
	OutcomeNoChange Outcome = "no_change"
	OutcomeFailed   Outcome = "failed"
	// OutcomeNeedsAttention: the run completed correctly and decided a human must
	// choose. An ambiguous migration it declined to guess at is a SUCCESSFUL
	// execution and an incomplete task.
	OutcomeNeedsAttention Outcome = "needs_attention"
	// OutcomeBlocked: the run could not start its real work (lock held, project
	// missing, no authorized runtime).
	OutcomeBlocked Outcome = "blocked"
	// OutcomeInterrupted: the process vanished mid-run — killed, crashed, or the
	// machine went down. Deliberately NOT retried automatically; see Reconcile.
	OutcomeInterrupted Outcome = "interrupted"
)

// Seen tracks whether a human has dealt with a run's result. Three states, not
// a boolean: a needs_attention run that scrolled past in a session banner has
// been SEEN, not dealt with, and collapsing those loses the distinction exactly
// where it matters most.
type Seen string

const (
	SeenUnseen       Seen = "unseen"
	SeenSeen         Seen = "seen"
	SeenAcknowledged Seen = "acknowledged"
)

// TriggerManual is the trigger kind for a run started by hand.
const TriggerManual = "manual"

// Run is one execution, with its inputs frozen at creation.
type Run struct {
	ID   string
	Task string

	// Frozen definition. Revision identifies it; Definition is the full YAML, so
	// a run stays readable even after the file is deleted.
	Revision   string
	Definition string

	// TriggerKind is manual/cron/every/at. TriggerID identifies the specific
	// OCCURRENCE and is the idempotency key: empty for manual runs (asking twice
	// means twice), unique per firing otherwise.
	TriggerKind string
	TriggerID   string

	Project    string
	Grants     []string
	Timeout    time.Duration
	MaxCostUSD float64

	// Where the inference ran. Requested and resolved are both frozen at
	// creation, so the choice stays explainable after the machine's state and
	// the user's authorizations have moved on.
	RuntimeRequested string
	RuntimeResolved  string
	ModelRequested   string
	ModelResolved    string
	CredSource       string
	AuthID           string
	AuthScope        string

	State   State
	Outcome Outcome
	Summary string
	Detail  string
	LogPath string

	// OccurredAt is the LOGICAL time this run stands for, which is not when it
	// started: a run_once recovery of Monday's occurrence executed on Wednesday
	// occurred-at Monday. That is what makes a recovered run explainable.
	OccurredAt time.Time
	// Backlog counts occurrences dropped when this one was created.
	Backlog int

	// Execution and verification, kept separable from Outcome.
	ExecStatus   ExecutionStatus
	VerifyStatus VerificationStatus
	Checks       string

	// Where the work happened. BaseRev is resolved at EXECUTION time, so a
	// recovered occurrence is honest about building on today's HEAD rather than
	// implying the repository was frozen when it was due.
	Worktree  string
	Branch    string
	BaseRev   string
	ResultRev string
	Changed   bool

	// What was published, and which artifacts this run created rather than
	// found already there.
	Remote        string
	CommitSHA     string
	PRNumber      int
	PRURL         string
	CreatedBranch bool
	CreatedCommit bool
	CreatedPR     bool

	Host string
	PID  int

	StartedAt   time.Time
	HeartbeatAt time.Time
	FinishedAt  time.Time
	Seen        Seen
}

// Terminal reports whether the run has finished.
func (r Run) Terminal() bool { return r.State == StateDone }

// OK reports whether a finished run did what it was supposed to. no_change is a
// success: the task ran correctly and there was nothing to do.
func (r Run) OK() bool { return r.Outcome == OutcomeSuccess || r.Outcome == OutcomeNoChange }

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id            TEXT PRIMARY KEY,
  task          TEXT NOT NULL,
  revision      TEXT NOT NULL,
  definition    TEXT NOT NULL,
  trigger_kind  TEXT NOT NULL,
  trigger_id    TEXT NOT NULL DEFAULT '',
  project       TEXT NOT NULL,
  grants        TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  timeout_ns    INTEGER NOT NULL DEFAULT 0,
  max_cost_usd  REAL NOT NULL DEFAULT 0,
  state         TEXT NOT NULL,
  outcome       TEXT NOT NULL DEFAULT '',
  summary       TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  log_path      TEXT NOT NULL DEFAULT '',
  host          TEXT NOT NULL DEFAULT '',
  pid           INTEGER NOT NULL DEFAULT 0,
  started_at    TEXT NOT NULL,
  heartbeat_at  TEXT NOT NULL DEFAULT '',
  finished_at   TEXT NOT NULL DEFAULT '',
  seen          TEXT NOT NULL DEFAULT 'unseen',
  -- How many occurrences were deliberately NOT run when this one was created.
  -- A collapsed backlog that is invisible is indistinguishable from a scheduler
  -- that quietly stopped working.
  backlog       INTEGER NOT NULL DEFAULT 0,
  occurred_at   TEXT NOT NULL DEFAULT '',
  -- Execution and verification are recorded SEPARATELY from the final outcome.
  -- A run whose agent finished cleanly and whose tests then failed is an
  -- execution success and a task failure; one enum cannot say that, and
  -- debugging without the distinction is guesswork.
  exec_status   TEXT NOT NULL DEFAULT '',
  verify_status TEXT NOT NULL DEFAULT '',
  checks        TEXT NOT NULL DEFAULT '',
  -- Where the work happened and what it produced.
  worktree      TEXT NOT NULL DEFAULT '',
  branch        TEXT NOT NULL DEFAULT '',
  base_rev      TEXT NOT NULL DEFAULT '',
  result_rev    TEXT NOT NULL DEFAULT '',
  changed       INTEGER NOT NULL DEFAULT 0,
  -- What the run published, and what it CREATED as opposed to reused. The
  -- distinction matters on a retry: reusing a branch is not making one.
  remote          TEXT NOT NULL DEFAULT '',
  commit_sha      TEXT NOT NULL DEFAULT '',
  pr_number       INTEGER NOT NULL DEFAULT 0,
  pr_url          TEXT NOT NULL DEFAULT '',
  created_branch  INTEGER NOT NULL DEFAULT 0,
  created_commit  INTEGER NOT NULL DEFAULT 0,
  created_pr      INTEGER NOT NULL DEFAULT 0,
  -- WHERE the inference ran, frozen before execution. Requested and resolved are
  -- both kept: "why did this run through Codex rather than hosted memcode" is
  -- only answerable if you can see what it asked for as well as what it got.
  runtime_requested TEXT NOT NULL DEFAULT '',
  runtime_resolved  TEXT NOT NULL DEFAULT '',
  model_requested   TEXT NOT NULL DEFAULT '',
  model_resolved    TEXT NOT NULL DEFAULT '',
  cred_source       TEXT NOT NULL DEFAULT '',
  auth_id           TEXT NOT NULL DEFAULT '',
  auth_scope        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_by_task ON runs(task, started_at DESC);
CREATE INDEX IF NOT EXISTS runs_by_seen ON runs(seen, started_at DESC);
-- THE idempotency boundary. A triggered occurrence may be dispatched more than
-- once (at-least-once delivery, a daemon restart, two racing processes); only
-- the first INSERT wins, and the loser learns the work is already accounted
-- for instead of doing it again. Manual runs carry an empty trigger_id and are
-- excluded: running a task by hand twice is a deliberate act, not a duplicate.
CREATE UNIQUE INDEX IF NOT EXISTS runs_occurrence
  ON runs(task, trigger_id) WHERE trigger_id <> '';
` + leaseSchema

// Store is the run ledger.
type Store struct{ db *sql.DB }

// DBPath is the ledger's location, alongside the rest of memcode's per-machine
// state.
func DBPath() (string, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tasks.db"), nil
}

// Open creates or opens the ledger. There is deliberately NO exclusive lock:
// the CLI, the daemon and spawned children all touch this concurrently, and WAL
// plus a busy timeout is what makes that safe. The gateway's own singleton lock
// exists to keep one inbox worker, which is a different problem.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// PRAGMAS IN THE DSN, not as statements afterwards.
	//
	// database/sql hands out a POOL. `db.Exec("PRAGMA busy_timeout=5000")` runs
	// on whichever connection happens to serve it and configures that one only;
	// every other connection the pool opens later gets the default of zero and
	// fails the instant it meets a writer, with SQLITE_BUSY.
	//
	// This ledger is the most concurrent thing memcode has — a run writing, its
	// heartbeat goroutine renewing a lease, a poll starting the next one, all
	// while a separate CLI process reads the inbox. It hid on a fast laptop and
	// showed up as "database is locked" under -race on CI.
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying task-run schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, pauseSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("creating task pause table: %w", err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate brings a ledger created by an earlier version forward.
//
// CREATE TABLE IF NOT EXISTS does nothing to a table that already exists, so a
// column added later never appears in an existing database — and every test
// using a fresh temp DB passes while the real one fails on first use. Each
// statement is additive and ignores "duplicate column", so running it against
// an already-current database is a no-op.
func migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE runs ADD COLUMN backlog INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN occurred_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN exec_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN verify_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN checks TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN worktree TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN branch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN base_rev TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN result_rev TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN changed INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN remote TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN commit_sha TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN pr_number INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN pr_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN created_branch INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN created_commit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN created_pr INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE runs ADD COLUMN runtime_requested TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN runtime_resolved TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN model_requested TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN model_resolved TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN cred_source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN auth_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE runs ADD COLUMN auth_scope TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("migrating task ledger: %w", err)
		}
	}
	return nil
}

// OpenDefault opens the ledger at its standard location.
func OpenDefault(ctx context.Context) (*Store, error) {
	path, err := DBPath()
	if err != nil {
		return nil, err
	}
	return Open(ctx, path)
}

func (s *Store) Close() error { return s.db.Close() }

// ErrOccupied means this trigger occurrence already has a run. The caller must
// NOT execute: someone else owns it.
var ErrOccupied = fmt.Errorf("this occurrence already has a run")

// NewID returns a sortable, unique run identity. Time-prefixed so a directory
// listing or an ORDER BY id reads chronologically without a join.
func NewID(now time.Time) string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("run_%s_%s", now.UTC().Format("20060102T150405"), hex.EncodeToString(b[:]))
}

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Create records a new run in StatePending, claiming its occurrence. It returns
// ErrOccupied when a triggered occurrence is already accounted for — that is the
// idempotency boundary doing its job, and the caller must treat it as "someone
// else has this", never as an error to retry through.
func (s *Store) Create(ctx context.Context, r Run) (Run, error) {
	if r.ID == "" {
		r.ID = NewID(time.Now())
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	r.State = StatePending
	if r.Seen == "" {
		r.Seen = SeenUnseen
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, task, revision, definition, trigger_kind, trigger_id, project,
		                  grants, timeout_ns, max_cost_usd, state, started_at, seen,
		                  backlog, occurred_at, runtime_requested, runtime_resolved,
		                  model_requested, model_resolved, cred_source, auth_id, auth_scope)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Task, r.Revision, r.Definition, r.TriggerKind, r.TriggerID, r.Project,
		strings.Join(r.Grants, ","), int64(r.Timeout), r.MaxCostUSD,
		string(r.State), ts(r.StartedAt), string(r.Seen), r.Backlog, ts(r.OccurredAt),
		r.RuntimeRequested, r.RuntimeResolved, r.ModelRequested, r.ModelResolved,
		r.CredSource, r.AuthID, r.AuthScope)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Run{}, ErrOccupied
		}
		return Run{}, err
	}
	return r, nil
}

// Claim moves a pending run to running and stamps the owning process. The
// UPDATE is conditional on the row still being pending, so two processes racing
// for the same run cannot both win: the loser sees claimed=false.
func (s *Store) Claim(ctx context.Context, id string, now time.Time) (bool, error) {
	host, _ := os.Hostname()
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET state=?, host=?, pid=?, heartbeat_at=?
		WHERE id=? AND state=?`,
		string(StateRunning), host, os.Getpid(), ts(now), id, string(StatePending))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Heartbeat proves the owning process is still alive. Reconcile uses its
// absence to tell a crashed run from a slow one.
func (s *Store) Heartbeat(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET heartbeat_at=? WHERE id=? AND state=?`, ts(now), id, string(StateRunning))
	return err
}

// Finish closes a run. Terminal and idempotent: a run already done stays as it
// was, so a late finisher cannot overwrite an interrupted verdict.
// Result is everything a finished run records beyond its outcome.
type Result struct {
	Outcome      Outcome
	Summary      string
	Detail       string
	LogPath      string
	ExecStatus   ExecutionStatus
	VerifyStatus VerificationStatus
	Checks       string
	Worktree     string
	Branch       string
	BaseRev      string
	ResultRev    string
	Changed      bool

	Remote        string
	CommitSHA     string
	PRNumber      int
	PRURL         string
	CreatedBranch bool
	CreatedCommit bool
	CreatedPR     bool

	// Escalation is what the run concluded about FUTURE runs, as distinct from
	// this one. Not persisted on the run row: its consequence is a task pause,
	// which is its own durable record.
	Escalation    Escalation
	EscalationWhy string
}

// FinishResult closes a run with its full structured verdict.
func (s *Store) FinishResult(ctx context.Context, id string, r Result, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `
		UPDATE runs SET exec_status=?, verify_status=?, checks=?, worktree=?, branch=?,
		                base_rev=?, result_rev=?, changed=?, remote=?, commit_sha=?,
		                pr_number=?, pr_url=?, created_branch=?, created_commit=?, created_pr=?
		WHERE id=? AND state<>?`,
		string(r.ExecStatus), string(r.VerifyStatus), r.Checks, r.Worktree, r.Branch,
		r.BaseRev, r.ResultRev, r.Changed, r.Remote, r.CommitSHA,
		r.PRNumber, r.PRURL, r.CreatedBranch, r.CreatedCommit, r.CreatedPR,
		id, string(StateDone)); err != nil {
		return err
	}
	return s.Finish(ctx, id, r.Outcome, r.Summary, r.Detail, r.LogPath, now)
}

func (s *Store) Finish(ctx context.Context, id string, outcome Outcome, summary, detail, logPath string, now time.Time) error {
	// detail APPENDS: a note recorded before the run started (why this occurrence
	// exists) must survive the outcome being written over the top of it.
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET state=?, outcome=?, summary=?, log_path=?, finished_at=?,
		                detail = CASE WHEN ? = '' THEN detail
		                              WHEN detail = '' THEN ?
		                              ELSE detail || char(10) || ? END
		WHERE id=? AND state<>?`,
		string(StateDone), string(outcome), summary, logPath, ts(now),
		detail, detail, detail, id, string(StateDone))
	return err
}

// Note records why a run exists — a recovered occurrence, a collapsed backlog —
// before it executes, so the explanation survives even if the run then crashes.
// Appends rather than replaces: a run may accumulate more than one note.
func (s *Store) Note(ctx context.Context, id, note string) error {
	if note == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET detail = CASE WHEN detail = '' THEN ? ELSE detail || char(10) || ? END
		WHERE id=?`, note, note, id)
	return err
}

// Get loads one run.
func (s *Store) Get(ctx context.Context, id string) (Run, error) {
	rows, err := s.query(ctx, `SELECT `+cols+` FROM runs WHERE id=?`, id)
	if err != nil {
		return Run{}, err
	}
	if len(rows) == 0 {
		return Run{}, fmt.Errorf("no run %s", id)
	}
	return rows[0], nil
}

// Recent returns the newest runs, optionally for one task.
func (s *Store) Recent(ctx context.Context, task string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	if task != "" {
		return s.query(ctx, `SELECT `+cols+` FROM runs WHERE task=? ORDER BY started_at DESC LIMIT ?`, task, limit)
	}
	return s.query(ctx, `SELECT `+cols+` FROM runs ORDER BY started_at DESC LIMIT ?`, limit)
}

// Active returns runs that are not finished, for one task.
func (s *Store) Active(ctx context.Context, task string) ([]Run, error) {
	return s.query(ctx, `SELECT `+cols+` FROM runs WHERE task=? AND state<>? ORDER BY started_at DESC`,
		task, string(StateDone))
}

// Unseen returns finished runs a human has not looked at yet, oldest first so a
// session banner reads in the order things happened.
func (s *Store) Unseen(ctx context.Context, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.query(ctx, `SELECT `+cols+` FROM runs WHERE state=? AND seen=? ORDER BY started_at ASC LIMIT ?`,
		string(StateDone), string(SeenUnseen), limit)
}

// MarkSeen advances unseen runs to seen. It never touches an acknowledged run
// and never moves backwards: showing someone a banner is not the same as them
// dealing with it, which is the whole reason this is not a boolean.
func (s *Store) MarkSeen(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE runs SET seen=? WHERE id=? AND seen=?`,
			string(SeenSeen), id, string(SeenUnseen)); err != nil {
			return err
		}
	}
	return nil
}

// Acknowledge records that a human actually dealt with a run.
func (s *Store) Acknowledge(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET seen=? WHERE id=?`, string(SeenAcknowledged), id)
	return err
}

// StaleAfter is how long a running row may go without a heartbeat before
// Reconcile calls it dead. Generous relative to the heartbeat interval so a
// loaded machine is never mistaken for a crashed one.
const StaleAfter = 2 * time.Minute

// Reconcile settles runs left behind by a process that died — a killed daemon,
// a crash, a machine that went down mid-run.
//
// The semantic is deliberate and narrow: such a run is FAILED as interrupted,
// never silently resumed and never silently re-run. A partially finished
// mutating run cannot be safely continued by a new process that did not see
// what the old one did, and re-running it automatically is exactly how
// at-least-once dispatch turns into duplicate side effects. If the work still
// needs doing, that is a NEW run with a new identity, which the ledger shows.
//
// A run is considered dead when its heartbeat is older than StaleAfter, or when
// it is owned by this host and its pid is gone (which is immediate and does not
// wait out the timeout).
func (s *Store) Reconcile(ctx context.Context, now time.Time) (int, error) {
	running, err := s.query(ctx, `SELECT `+cols+` FROM runs WHERE state=?`, string(StateRunning))
	if err != nil {
		return 0, err
	}
	host, _ := os.Hostname()
	n := 0
	for _, r := range running {
		dead := false
		switch {
		case r.Host == host && r.PID > 0 && !processAlive(r.PID):
			dead = true
		case r.HeartbeatAt.IsZero() && now.Sub(r.StartedAt) > StaleAfter:
			dead = true
		case !r.HeartbeatAt.IsZero() && now.Sub(r.HeartbeatAt) > StaleAfter:
			dead = true
		}
		if !dead {
			continue
		}
		if err := s.Finish(ctx, r.ID, OutcomeInterrupted,
			"the process running this task exited before it finished",
			"Not resumed and not re-run automatically: a partially completed run cannot be "+
				"safely continued by a process that did not see what it did. Start it again "+
				"with `memcode task run` if the work still needs doing.",
			r.LogPath, now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

const cols = `id, task, revision, definition, trigger_kind, trigger_id, project, grants,
	timeout_ns, max_cost_usd, state, outcome, summary, detail, log_path,
	host, pid, started_at, heartbeat_at, finished_at, seen, backlog, occurred_at,
	exec_status, verify_status, checks, worktree, branch, base_rev, result_rev, changed,
	remote, commit_sha, pr_number, pr_url, created_branch, created_commit, created_pr,
	runtime_requested, runtime_resolved, model_requested, model_resolved, cred_source,
	auth_id, auth_scope`

func (s *Store) query(ctx context.Context, q string, args ...any) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var grants, started, heartbeat, finished, occurred string
		var timeoutNS int64
		if err := rows.Scan(&r.ID, &r.Task, &r.Revision, &r.Definition, &r.TriggerKind, &r.TriggerID,
			&r.Project, &grants, &timeoutNS, &r.MaxCostUSD, &r.State,
			&r.Outcome, &r.Summary, &r.Detail, &r.LogPath, &r.Host, &r.PID,
			&started, &heartbeat, &finished, &r.Seen, &r.Backlog, &occurred,
			&r.ExecStatus, &r.VerifyStatus, &r.Checks, &r.Worktree, &r.Branch,
			&r.BaseRev, &r.ResultRev, &r.Changed,
			&r.Remote, &r.CommitSHA, &r.PRNumber, &r.PRURL,
			&r.CreatedBranch, &r.CreatedCommit, &r.CreatedPR,
			&r.RuntimeRequested, &r.RuntimeResolved, &r.ModelRequested, &r.ModelResolved,
			&r.CredSource, &r.AuthID, &r.AuthScope); err != nil {
			return nil, err
		}
		if grants != "" {
			r.Grants = strings.Split(grants, ",")
		}
		r.Timeout = time.Duration(timeoutNS)
		r.StartedAt, r.HeartbeatAt, r.FinishedAt = parseTS(started), parseTS(heartbeat), parseTS(finished)
		r.OccurredAt = parseTS(occurred)
		out = append(out, r)
	}
	return out, rows.Err()
}
