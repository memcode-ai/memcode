package autonomy

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

//go:embed migrations/002_interactions.sql
var migration002 string

//go:embed migrations/003_action_job_id.sql
var migration003 string

// migrations is the ordered schema history. Version 1 is the base schema; later
// entries are additive ALTER/CREATE statements. Never edit a shipped entry.
var migrations = []string{schema, migration002, migration003}

type Store struct{ db *sql.DB }

// DB exposes the underlying handle for store-internal submodules (same
// package); external callers use Store methods only.
func (s *Store) DB() *sql.DB { return s.db }

func InitializeHome(home string) error {
	for _, entry := range []string{"policies", "workspace/generated", "workspace/scratch", "runs", "workers", ".memcode/jobs", ".memcode/sessions"} {
		if err := os.MkdirAll(filepath.Join(home, entry), 0o700); err != nil {
			return err
		}
	}
	return nil
}

func Open(ctx context.Context, home string) (*Store, error) {
	if err := InitializeHome(home); err != nil {
		return nil, fmt.Errorf("initialize agent home: %w", err)
	}
	path := filepath.Join(home, "personal.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("personal agent schema version %d is newer than supported version %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, migrations[i]); err == nil {
			_, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("applying autonomous agent migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) CreateObjective(ctx context.Context, o Objective) error {
	now := time.Now().UTC()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = now
	}
	if o.Status == "" {
		o.Status = "draft"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO objectives(id,description,success_criteria,status,priority,created_at,updated_at,review_at) VALUES(?,?,?,?,?,?,?,?)`, o.ID, o.Description, o.SuccessCriteria, o.Status, o.Priority, stamp(o.CreatedAt), stamp(o.UpdatedAt), nullableTime(o.ReviewAt))
	return err
}

func (s *Store) ListObjectives(ctx context.Context) ([]Objective, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,description,success_criteria,status,priority,created_at,updated_at,review_at FROM objectives ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Objective
	for rows.Next() {
		var o Objective
		var created, updated string
		var review sql.NullString
		if err := rows.Scan(&o.ID, &o.Description, &o.SuccessCriteria, &o.Status, &o.Priority, &created, &updated, &review); err != nil {
			return nil, err
		}
		o.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if review.Valid {
			v, _ := time.Parse(time.RFC3339Nano, review.String)
			o.ReviewAt = &v
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) SetObjectiveStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE objectives SET status=?,updated_at=? WHERE id=?`, status, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("objective %q not found", id)
	}
	return nil
}

// SetObjectiveText updates an objective's description (the user-authored goal).
func (s *Store) SetObjectiveText(ctx context.Context, id, description string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE objectives SET description=?,updated_at=? WHERE id=?`, description, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("objective %q not found", id)
	}
	return nil
}

func (s *Store) StatusSummary(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, table := range []string{"objectives", "subgoals", "runs", "triggers", "policies", "resources", "actions", "generated_items", "notifications"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			return nil, err
		}
		out[table] = n
	}
	return out, nil
}

func (s *Store) RevokeResources(ctx context.Context, objectiveID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE resources SET status='revoked',updated_at=? WHERE objective_id=? AND status='active'`, stamp(time.Now().UTC()), objectiveID)
	return err
}
func (s *Store) CancelPendingNotifications(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE notifications SET status='cancelled',updated_at=? WHERE status='pending'`, stamp(time.Now().UTC()))
	return err
}
func (s *Store) ResolveUncertainAction(ctx context.Context, id string, status ActionStatus) error {
	if status != ActionSucceeded && status != ActionFailed && status != ActionCancelled {
		return fmt.Errorf("invalid reconciliation status")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,updated_at=? WHERE id=? AND status='uncertain'`, status, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("uncertain action %q not found", id)
	}
	return nil
}
func (s *Store) RecoverableRuns(ctx context.Context) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,subgoal_id,parent_run_id,session_id,status,created_at,updated_at FROM runs WHERE status IN ('running','waiting','resumable')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var sub, parent, session sql.NullString
		var created, updated string
		if err := rows.Scan(&r.ID, &r.ObjectiveID, &sub, &parent, &session, &r.Status, &created, &updated); err != nil {
			return nil, err
		}
		r.SubgoalID = sub.String
		r.ParentRunID = parent.String
		r.SessionID = session.String
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteObjective(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM objectives WHERE id=?`, id)
	return err
}

func (s *Store) GetObjective(ctx context.Context, id string) (Objective, bool, error) {
	var o Objective
	var created, updated string
	var review sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,description,success_criteria,status,priority,created_at,updated_at,review_at FROM objectives WHERE id=?`, id).Scan(&o.ID, &o.Description, &o.SuccessCriteria, &o.Status, &o.Priority, &created, &updated, &review)
	if err == sql.ErrNoRows {
		return Objective{}, false, nil
	}
	if err != nil {
		return Objective{}, false, err
	}
	o.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	o.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if review.Valid {
		t, _ := time.Parse(time.RFC3339Nano, review.String)
		o.ReviewAt = &t
	}
	return o, true, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}
