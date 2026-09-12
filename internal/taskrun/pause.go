package taskrun

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// A task that cannot do its job any more should STOP, not keep failing weekly.
//
// The three states are genuinely different and collapsing them loses the
// distinction exactly when it matters:
//
//	failed           this run failed; the task itself is still sound
//	needs_attention  this run hit something it could not safely resolve
//	paused           the task has suspended itself until a person decides
//
// The rule that separates the second from the third: if continuing would
// require inventing new intent, expanding authority, or making a consequential
// architectural choice, the condition will still be there next week. Running
// into it every Monday and reporting the same failure is not resilience, it is
// noise that trains people to ignore the inbox.
//
// Pausing is deliberately NOT written into the task's YAML. That file is the
// user's, reviewed and committed; a machine editing it to record a local
// operational state would put churn in their repo and would travel to other
// clones where it is not true. The state belongs to the ledger.

// Escalation is what a run concluded about the task's future, as distinct from
// what it concluded about itself.
type Escalation string

const (
	// EscalateNone: nothing to say. The default, and the common case.
	EscalateNone Escalation = ""
	// EscalateRetry: a transient condition — an upstream outage, a flaky
	// network. The task is fine; this run was unlucky. Never pauses, because a
	// temporary problem that suspends an automation forever is its own bug.
	EscalateRetry Escalation = "retry_later"
	// EscalateAttention: this run needs a human to look, but the next one is
	// still worth running.
	EscalateAttention Escalation = "needs_attention"
	// EscalatePause: the condition will recur until somebody decides something.
	EscalatePause Escalation = "pause_task"
)

// ParseEscalation reads the escalation line a run was asked to end with.
//
// This is a DECLARED protocol, not error-string matching: the agent is told to
// state its conclusion in a fixed form and this reads that form. The judgement
// — is this transient, or does it need a person, or will it recur forever — is
// the model's, made with the whole situation in view. Guessing it from the
// shape of an error message is what this exists to avoid.
func ParseEscalation(out string) (Escalation, string) {
	const marker = "ESCALATION:"
	var kind Escalation
	var reason string
	// LAST wins: an agent that reconsiders after trying something else should
	// be taken at its final word, not its first.
	for _, line := range strings.Split(out, "\n") {
		i := strings.Index(strings.ToUpper(line), marker)
		if i < 0 {
			continue
		}
		rest := strings.TrimSpace(line[i+len(marker):])
		word, why, _ := strings.Cut(rest, " ")
		word = strings.Trim(strings.ToLower(strings.TrimSpace(word)), ".,:—-")
		switch Escalation(word) {
		case EscalateRetry, EscalateAttention, EscalatePause:
			kind, reason = Escalation(word), strings.TrimSpace(strings.TrimLeft(why, "—-: "))
		case "continue", "none":
			kind, reason = EscalateNone, ""
		}
	}
	return kind, reason
}

// Pause is a task's suspension, with the evidence needed to resolve it.
type Pause struct {
	Task    string
	Project string
	Reason  string
	RunID   string
	Since   time.Time
}

const pauseSchema = `
CREATE TABLE IF NOT EXISTS task_pauses (
  task     TEXT NOT NULL,
  project  TEXT NOT NULL DEFAULT '',
  reason   TEXT NOT NULL DEFAULT '',
  run_id   TEXT NOT NULL DEFAULT '',
  since    TEXT NOT NULL,
  PRIMARY KEY (task, project)
);`

// Pause suspends a task's unattended execution. Idempotent on the FIRST reason:
// re-pausing an already-paused task must not overwrite the original diagnosis
// with a later, vaguer one.
func (s *Store) Pause(ctx context.Context, p Pause) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO task_pauses (task, project, reason, run_id, since) VALUES (?,?,?,?,?)`,
		p.Task, p.Project, p.Reason, p.RunID, p.Since.UTC().Format(time.RFC3339Nano))
	return err
}

// Resume lifts a suspension. Reports whether one was actually there.
func (s *Store) Resume(ctx context.Context, taskName, project string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM task_pauses WHERE task=? AND project=?`, taskName, project)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PausedTask reports a task's suspension, if it has one.
func (s *Store) PausedTask(ctx context.Context, taskName, project string) (Pause, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT task, project, reason, run_id, since FROM task_pauses WHERE task=? AND project=?`,
		taskName, project)
	p, err := scanPause(row)
	if err == sql.ErrNoRows {
		return Pause{}, false, nil
	}
	if err != nil {
		return Pause{}, false, err
	}
	return p, true, nil
}

// Paused lists every suspended task, oldest first: a responsibility memcode
// stopped fulfilling longest ago is the one most worth raising.
func (s *Store) Paused(ctx context.Context) ([]Pause, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT task, project, reason, run_id, since FROM task_pauses ORDER BY since ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pause
	for rows.Next() {
		p, err := scanPause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanPause(r scanner) (Pause, error) {
	var p Pause
	var since string
	if err := r.Scan(&p.Task, &p.Project, &p.Reason, &p.RunID, &since); err != nil {
		return Pause{}, err
	}
	p.Since, _ = time.Parse(time.RFC3339Nano, since)
	return p, nil
}

// String renders a pause for a human deciding whether to deal with it now.
func (p Pause) String() string {
	return fmt.Sprintf("%s — paused: %s", p.Task, firstLine(p.Reason))
}
