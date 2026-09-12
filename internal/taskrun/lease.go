package taskrun

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/memcode-ai/memcode/internal/task"
)

// Concurrency control is a DURABLE LEASE, not an in-process mutex.
//
// The daemon is not the only thing that runs tasks — `memcode task run` does
// too, from a different process — so a mutex protects nothing across the pair.
// Worse, a mutex dies with its process: a gateway killed mid-run would leave no
// trace, and the next process would have no principled way to tell "someone is
// working on this" from "someone was working on this when the power went out".
//
// So ownership is a row, held by (run, host, pid) and kept alive by the same
// heartbeat the run itself uses. A lease whose owner stopped heartbeating, or
// whose pid is gone on this host, is expired and can be taken — the identical
// liveness rule Reconcile uses for runs, deliberately, so there is one notion of
// "that process is gone" in the system rather than two that can disagree.

const leaseSchema = `
CREATE TABLE IF NOT EXISTS leases (
  key          TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL,
  task         TEXT NOT NULL,
  host         TEXT NOT NULL,
  pid          INTEGER NOT NULL,
  acquired_at  TEXT NOT NULL,
  heartbeat_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS trigger_state (
  task            TEXT NOT NULL,
  trigger_key     TEXT NOT NULL,
  last_occurrence TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  PRIMARY KEY (task, trigger_key)
);
`

// LeaseKey is the resource a run needs exclusive use of.
//
// Read-only runs return "" and take no lease at all: they change nothing, so two
// of them in a repo cannot interfere, and serializing them would only make an
// audit task block a dependency upgrade for no reason.
//
// A mutating run keys on its RESOLVED PROJECT, so every mutating task in a repo
// serializes against the others. Worktree isolation may safely relax this later,
// but one mutating run per project is the conservative default and the right
// place to start.
func LeaseKey(t task.Task, project string) string {
	if !t.MayMutate() {
		return ""
	}
	switch t.Concurrency.Key {
	case "", task.ConcurrencyKeyProject:
		return "project:" + project
	default:
		return "custom:" + t.Concurrency.Key
	}
}

// LeaseKeys is every resource one run must hold exclusively.
//
// A cross-project run mutates several checkouts, and leasing only the one it
// was anchored in leaves the others open to a second run working in them at the
// same time — which is the failure the lease exists to prevent, just moved one
// project to the left. A custom concurrency key is one key by definition and
// stays one key.
func LeaseKeys(t task.Task, project string) []string {
	if !t.MayMutate() {
		return nil
	}
	if t.Concurrency.Key != "" && t.Concurrency.Key != task.ConcurrencyKeyProject {
		return []string{"custom:" + t.Concurrency.Key}
	}
	// Sorted, so two runs over the same projects always take their locks in the
	// same order and cannot deadlock against each other.
	targets := append([]string(nil), t.TargetsFrom(project)...)
	sort.Strings(targets)
	out := make([]string, 0, len(targets))
	for _, p := range targets {
		out = append(out, "project:"+p)
	}
	return out
}

// Lease is a held claim on a resource.
type Lease struct {
	Key         string
	RunID       string
	Task        string
	Host        string
	PID         int
	AcquiredAt  time.Time
	HeartbeatAt time.Time
}

// Expired reports whether a lease's owner looks gone, by the same rule
// Reconcile applies to runs.
func (l Lease) Expired(now time.Time, host string) bool {
	if l.Host == host && l.PID > 0 && !processAlive(l.PID) {
		return true
	}
	ref := l.HeartbeatAt
	if ref.IsZero() {
		ref = l.AcquiredAt
	}
	return now.Sub(ref) > StaleAfter
}

// ErrLeaseHeld means someone else is working on this resource right now.
type ErrLeaseHeld struct{ Holder Lease }

func (e ErrLeaseHeld) Error() string {
	return fmt.Sprintf("%s is already held by run %s (task %s) on %s",
		e.Holder.Key, e.Holder.RunID, e.Holder.Task, e.Holder.Host)
}

// Acquire takes a lease, stealing one whose owner is gone. An empty key is a
// no-op success: a read-only run needs no exclusivity.
func (s *Store) Acquire(ctx context.Context, key, runID, taskName string, now time.Time) error {
	if key == "" {
		return nil
	}
	host, _ := os.Hostname()

	// Fast path: nobody holds it.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO leases (key, run_id, task, host, pid, acquired_at, heartbeat_at)
		VALUES (?,?,?,?,?,?,?)`,
		key, runID, taskName, host, os.Getpid(), ts(now), ts(now))
	if err == nil {
		return nil
	}

	held, ok, gerr := s.lease(ctx, key)
	if gerr != nil {
		return gerr
	}
	if !ok {
		return err // the insert failed for some reason other than a holder
	}
	if held.RunID == runID {
		return nil // already ours; re-acquiring is idempotent
	}
	if !held.Expired(now, host) {
		return ErrLeaseHeld{Holder: held}
	}
	// The holder is gone. Steal it, but only if it is STILL the same dead holder
	// — otherwise a live process that acquired in between would be evicted.
	res, uerr := s.db.ExecContext(ctx, `
		UPDATE leases SET run_id=?, task=?, host=?, pid=?, acquired_at=?, heartbeat_at=?
		WHERE key=? AND run_id=?`,
		runID, taskName, host, os.Getpid(), ts(now), ts(now), key, held.RunID)
	if uerr != nil {
		return uerr
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrLeaseHeld{Holder: held}
	}
	return nil
}

// Renew keeps a held lease alive.
func (s *Store) Renew(ctx context.Context, key, runID string, now time.Time) error {
	if key == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE leases SET heartbeat_at=? WHERE key=? AND run_id=?`, ts(now), key, runID)
	return err
}

// Release drops a lease, but only if this run still holds it — a run that was
// already evicted as dead must not delete its successor's claim.
func (s *Store) Release(ctx context.Context, key, runID string) error {
	if key == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE key=? AND run_id=?`, key, runID)
	return err
}

func (s *Store) lease(ctx context.Context, key string) (Lease, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT key, run_id, task, host, pid, acquired_at, heartbeat_at FROM leases WHERE key=?`, key)
	var l Lease
	var acquired, heartbeat string
	if err := row.Scan(&l.Key, &l.RunID, &l.Task, &l.Host, &l.PID, &acquired, &heartbeat); err != nil {
		return Lease{}, false, nil
	}
	l.AcquiredAt, l.HeartbeatAt = parseTS(acquired), parseTS(heartbeat)
	return l, true, nil
}

// Watermark returns the newest occurrence already accounted for on a trigger.
func (s *Store) Watermark(ctx context.Context, taskName, triggerKey string) (time.Time, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT last_occurrence FROM trigger_state WHERE task=? AND trigger_key=?`, taskName, triggerKey)
	var v string
	if err := row.Scan(&v); err != nil {
		return time.Time{}, nil // never seen
	}
	return parseTS(v), nil
}

// SetWatermark records an occurrence as accounted for — whether it ran or was
// deliberately dropped. Advancing on a drop is what stops a skipped occurrence
// being rediscovered as missed on the next poll, forever.
//
// It never moves backwards, so an out-of-order poll cannot rewind the series.
func (s *Store) SetWatermark(ctx context.Context, taskName, triggerKey string, at, now time.Time) error {
	cur, err := s.Watermark(ctx, taskName, triggerKey)
	if err != nil {
		return err
	}
	if !cur.IsZero() && !at.After(cur) {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO trigger_state (task, trigger_key, last_occurrence, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(task, trigger_key) DO UPDATE SET last_occurrence=excluded.last_occurrence,
		                                             updated_at=excluded.updated_at`,
		taskName, triggerKey, ts(at), ts(now))
	return err
}
