package autonomy

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type MissedRunPolicy string

const (
	MissedSkip    MissedRunPolicy = "skip"
	MissedRunOnce MissedRunPolicy = "run_once"
	MissedCatchUp MissedRunPolicy = "catch_up"
)

// NextDue resolves when a trigger should next fire.
//
// The only kinds here are the ones an AGENT writes for itself from inside a run
// (schedule_wake: "come back in 45 minutes"), which are always a single future
// instant. Recurring cadence a HUMAN configures is not a trigger at all — it is
// an ordinary gateway schedule delivering to agent:<name>, parsed and validated
// once by gwconfig (see ValidateScheduleSpec/BuildSchedule).
//
// That split is deliberate: this package used to carry its own interval/cron
// parsing, which meant two cron implementations in one binary and two places
// for scheduling rules to disagree. Adding kinds back here would rebuild the
// second scheduler — internal/guard's TestSingleCronParser fails if it happens.
func NextDue(kind, spec string, after time.Time) (time.Time, error) {
	switch kind {
	case "manual":
		return time.Time{}, nil
	case "one_shot", "next_wake":
		return time.Parse(time.RFC3339, spec)
	default:
		return time.Time{}, fmt.Errorf("unknown wake kind %q (an agent's self-scheduled wake is one_shot or next_wake; recurring cadence belongs in gw_schedule)", kind)
	}
}

func (s *Store) CreateTrigger(ctx context.Context, t Trigger) error {
	now := time.Now().UTC()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	if t.Status == "" {
		t.Status = "enabled"
	}
	if t.MissedRunPolicy == "" {
		t.MissedRunPolicy = string(MissedSkip)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO triggers(id,objective_id,kind,spec,missed_run_policy,status,next_due_at,last_fired_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, t.ID, t.ObjectiveID, t.Kind, t.Spec, t.MissedRunPolicy, t.Status, nullableTime(t.NextDueAt), nullableTime(t.LastFiredAt), stamp(t.CreatedAt), stamp(t.UpdatedAt))
	return err
}

func (s *Store) ListTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,kind,spec,missed_run_policy,status,next_due_at,last_fired_at,created_at,updated_at FROM triggers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		var t Trigger
		var next, last sql.NullString
		var created, updated string
		if err := rows.Scan(&t.ID, &t.ObjectiveID, &t.Kind, &t.Spec, &t.MissedRunPolicy, &t.Status, &next, &last, &created, &updated); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if next.Valid {
			v, _ := time.Parse(time.RFC3339Nano, next.String)
			t.NextDueAt = &v
		}
		if last.Valid {
			v, _ := time.Parse(time.RFC3339Nano, last.String)
			t.LastFiredAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DueTriggers returns only enabled triggers whose next_due_at has passed,
// filtered in SQL rather than pulling every trigger row (including completed
// ones) and filtering in Go on every poll.
func (s *Store) DueTriggers(ctx context.Context, now time.Time) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,kind,spec,missed_run_policy,status,next_due_at,last_fired_at,created_at,updated_at FROM triggers WHERE status='enabled' AND next_due_at IS NOT NULL AND next_due_at<=? ORDER BY created_at`, stamp(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		var t Trigger
		var next, last sql.NullString
		var created, updated string
		if err := rows.Scan(&t.ID, &t.ObjectiveID, &t.Kind, &t.Spec, &t.MissedRunPolicy, &t.Status, &next, &last, &created, &updated); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if next.Valid {
			v, _ := time.Parse(time.RFC3339Nano, next.String)
			t.NextDueAt = &v
		}
		if last.Valid {
			v, _ := time.Parse(time.RFC3339Nano, last.String)
			t.LastFiredAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) ClaimDueTrigger(ctx context.Context, id string, now time.Time) (Trigger, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Trigger{}, false, err
	}
	defer tx.Rollback()
	var t Trigger
	var next, last sql.NullString
	var created, updated string
	err = tx.QueryRowContext(ctx, `SELECT id,objective_id,kind,spec,missed_run_policy,status,next_due_at,last_fired_at,created_at,updated_at FROM triggers WHERE id=? AND status='enabled' AND next_due_at IS NOT NULL AND next_due_at<=?`, id, stamp(now)).Scan(&t.ID, &t.ObjectiveID, &t.Kind, &t.Spec, &t.MissedRunPolicy, &t.Status, &next, &last, &created, &updated)
	if err == sql.ErrNoRows {
		return Trigger{}, false, nil
	}
	if err != nil {
		return Trigger{}, false, err
	}
	// Every self-scheduled wake is a single instant, so firing one completes it.
	// (Recurring cadence never reaches this table — it is a gateway schedule.)
	// The `last_fired_at IS ?` guard makes the claim atomic: a second gateway
	// process racing on the same row updates zero rows and backs off.
	fired := now.UTC()
	t.Status = "completed"
	res, err := tx.ExecContext(ctx, `UPDATE triggers SET status=?,last_fired_at=?,next_due_at=NULL,updated_at=? WHERE id=? AND last_fired_at IS ?`, t.Status, stamp(fired), stamp(fired), id, nullSQL(last))
	if err != nil {
		return Trigger{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Trigger{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return Trigger{}, false, err
	}
	t.LastFiredAt = &fired
	return t, true, nil
}

func nullSQL(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}
