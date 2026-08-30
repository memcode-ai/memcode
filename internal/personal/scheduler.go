package personal

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

type MissedRunPolicy string

const (
	MissedSkip    MissedRunPolicy = "skip"
	MissedRunOnce MissedRunPolicy = "run_once"
	MissedCatchUp MissedRunPolicy = "catch_up"
)

func NextDue(kind, spec string, after time.Time) (time.Time, error) {
	switch kind {
	case "manual":
		return time.Time{}, nil
	case "interval":
		d, err := time.ParseDuration(spec)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("invalid interval %q", spec)
		}
		return after.Add(d), nil
	case "cron":
		sch, err := cron.ParseStandard(spec)
		if err != nil {
			return time.Time{}, err
		}
		return sch.Next(after), nil
	case "one_shot", "next_wake":
		return time.Parse(time.RFC3339, spec)
	default:
		return time.Time{}, fmt.Errorf("unknown trigger kind %q", kind)
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
	fired := now.UTC()
	var newNext any
	if t.Kind != "one_shot" && t.Kind != "next_wake" {
		n, e := NextDue(t.Kind, t.Spec, fired)
		if e != nil {
			return Trigger{}, false, e
		}
		newNext = stamp(n)
	} else {
		t.Status = "completed"
	}
	res, err := tx.ExecContext(ctx, `UPDATE triggers SET status=?,last_fired_at=?,next_due_at=?,updated_at=? WHERE id=? AND last_fired_at IS ?`, t.Status, stamp(fired), newNext, stamp(fired), id, nullSQL(last))
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
