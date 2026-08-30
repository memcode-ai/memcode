package autonomy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func jsonOr(raw json.RawMessage, fallback string) string {
	if len(raw) == 0 {
		return fallback
	}
	return string(raw)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --- Subgoals ---

func (s *Store) UpsertSubgoal(ctx context.Context, g Subgoal) error {
	now := time.Now().UTC()
	if g.Status == "" {
		g.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO subgoals(id,objective_id,parent_id,description,status,priority,rationale,dependencies_json,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET description=excluded.description,status=excluded.status,priority=excluded.priority,rationale=excluded.rationale,updated_at=excluded.updated_at`,
		g.ID, g.ObjectiveID, nullStr(g.ParentID), g.Description, g.Status, g.Priority, g.Rationale, jsonOr(g.Dependencies, "[]"), stamp(now), stamp(now))
	return err
}

func (s *Store) ListSubgoals(ctx context.Context, objectiveID string) ([]Subgoal, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,COALESCE(parent_id,''),description,status,priority,rationale,dependencies_json,created_at,updated_at FROM subgoals WHERE objective_id=? ORDER BY priority DESC, created_at`, objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subgoal
	for rows.Next() {
		var g Subgoal
		var created, updated, deps string
		if err := rows.Scan(&g.ID, &g.ObjectiveID, &g.ParentID, &g.Description, &g.Status, &g.Priority, &g.Rationale, &deps, &created, &updated); err != nil {
			return nil, err
		}
		g.Dependencies = json.RawMessage(deps)
		g.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		g.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) SetSubgoalStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE subgoals SET status=?,updated_at=? WHERE id=?`, status, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("subgoal %q not found", id)
	}
	return nil
}

// --- Runs ---

func (s *Store) CreateRun(ctx context.Context, r Run) error {
	now := time.Now().UTC()
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `INSERT INTO runs(id,objective_id,subgoal_id,parent_run_id,session_id,envelope_json,status,outcome_json,evidence_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ObjectiveID, nullStr(r.SubgoalID), nullStr(r.ParentRunID), nullStr(r.SessionID), jsonOr(r.Envelope, "{}"), r.Status, string(r.Outcome), string(r.Evidence), stamp(r.CreatedAt), stamp(r.UpdatedAt))
	return err
}

func (s *Store) UpdateRunStatus(ctx context.Context, id, status string, outcome json.RawMessage) error {
	res, err := s.db.ExecContext(ctx, `UPDATE runs SET status=?,outcome_json=?,updated_at=? WHERE id=?`, status, string(outcome), stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("run %q not found", id)
	}
	return nil
}

func (s *Store) ListRuns(ctx context.Context, objectiveID string, limit int) ([]Run, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,COALESCE(subgoal_id,''),COALESCE(parent_run_id,''),COALESCE(session_id,''),envelope_json,status,COALESCE(outcome_json,''),COALESCE(evidence_json,''),created_at,updated_at FROM runs WHERE objective_id=? ORDER BY created_at DESC LIMIT ?`, objectiveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var env, outcome, evidence, created, updated string
		if err := rows.Scan(&r.ID, &r.ObjectiveID, &r.SubgoalID, &r.ParentRunID, &r.SessionID, &env, &r.Status, &outcome, &evidence, &created, &updated); err != nil {
			return nil, err
		}
		r.Envelope, r.Outcome, r.Evidence = json.RawMessage(env), json.RawMessage(outcome), json.RawMessage(evidence)
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Policies ---

func (s *Store) InsertPolicy(ctx context.Context, p Policy) error {
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.Status == "" {
		p.Status = "draft"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO policies(id,objective_id,version,document_json,hash,status,approved_at,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.ObjectiveID, p.Version, string(p.Document), p.Hash, p.Status, formatTimePtr(p.ApprovedAt), stamp(p.CreatedAt))
	return err
}

func (s *Store) ApprovePolicy(ctx context.Context, hash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := stamp(time.Now().UTC())
	var objectiveID string
	if err := tx.QueryRowContext(ctx, `SELECT objective_id FROM policies WHERE hash=?`, hash).Scan(&objectiveID); err != nil {
		return fmt.Errorf("policy %q not found", hash)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE policies SET status='superseded' WHERE objective_id=? AND status='approved'`, objectiveID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE policies SET status='approved',approved_at=? WHERE hash=? AND status='draft'`, now, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("policy %q is not a draft (already approved or unknown)", hash)
	}
	return tx.Commit()
}

func (s *Store) ApprovedPolicy(ctx context.Context, objectiveID string) (Policy, bool, error) {
	var p Policy
	var doc, created string
	var approved sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,objective_id,version,document_json,hash,status,approved_at,created_at FROM policies WHERE objective_id=? AND status='approved'`, objectiveID).
		Scan(&p.ID, &p.ObjectiveID, &p.Version, &doc, &p.Hash, &p.Status, &approved, &created)
	if err == sql.ErrNoRows {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, err
	}
	p.Document = json.RawMessage(doc)
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if approved.Valid {
		t, _ := time.Parse(time.RFC3339Nano, approved.String)
		p.ApprovedAt = &t
	}
	return p, true, nil
}

func (s *Store) ListPolicies(ctx context.Context, objectiveID string) ([]Policy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,version,document_json,hash,status,approved_at,created_at FROM policies WHERE objective_id=? ORDER BY version`, objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		var doc, created string
		var approved sql.NullString
		if err := rows.Scan(&p.ID, &p.ObjectiveID, &p.Version, &doc, &p.Hash, &p.Status, &approved, &created); err != nil {
			return nil, err
		}
		p.Document = json.RawMessage(doc)
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if approved.Valid {
			t, _ := time.Parse(time.RFC3339Nano, approved.String)
			p.ApprovedAt = &t
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) NextPolicyVersion(ctx context.Context, objectiveID string) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM policies WHERE objective_id=?`, objectiveID).Scan(&v)
	return v, err
}

func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return stamp(*t)
}

// --- Resources ---

func (s *Store) InsertResource(ctx context.Context, r Resource) error {
	now := time.Now().UTC()
	if r.Status == "" {
		r.Status = "active"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO resources(id,objective_id,type,locator,access_mode,constraints_json,authorization_source,policy_hash,expires_at,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.ObjectiveID, r.Type, r.Locator, r.AccessMode, jsonOr(r.Constraints, "{}"), r.AuthorizationSource, r.PolicyHash, formatTimePtr(r.ExpiresAt), r.Status, stamp(now), stamp(now))
	return err
}

func (s *Store) ListResources(ctx context.Context, objectiveID string) ([]Resource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,type,locator,access_mode,constraints_json,authorization_source,policy_hash,expires_at,status,created_at,updated_at FROM resources WHERE objective_id=? ORDER BY created_at`, objectiveID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Resource
	for rows.Next() {
		var r Resource
		var cons, created, updated string
		var expires sql.NullString
		if err := rows.Scan(&r.ID, &r.ObjectiveID, &r.Type, &r.Locator, &r.AccessMode, &cons, &r.AuthorizationSource, &r.PolicyHash, &expires, &r.Status, &created, &updated); err != nil {
			return nil, err
		}
		r.Constraints = json.RawMessage(cons)
		if expires.Valid {
			t, _ := time.Parse(time.RFC3339Nano, expires.String)
			r.ExpiresAt = &t
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SetResourceStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE resources SET status=?,updated_at=? WHERE id=?`, status, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("resource %q not found", id)
	}
	return nil
}

// --- Actions (list) ---

func (s *Store) ListActions(ctx context.Context, objectiveID string, limit int) ([]Action, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,objective_id,COALESCE(subgoal_id,''),COALESCE(run_id,''),kind,target,consequence_class,policy_hash,request_json,COALESCE(idempotency_key,''),status,COALESCE(result_json,''),COALESCE(evidence_json,''),COALESCE(job_id,''),created_at,updated_at FROM actions WHERE objective_id=? ORDER BY created_at DESC LIMIT ?`, objectiveID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Action
	for rows.Next() {
		var a Action
		var req, result, evidence, created, updated string
		if err := rows.Scan(&a.ID, &a.ObjectiveID, &a.SubgoalID, &a.RunID, &a.Kind, &a.Target, &a.ConsequenceClass, &a.PolicyHash, &req, &a.IdempotencyKey, &a.Status, &result, &evidence, &a.JobID, &created, &updated); err != nil {
			return nil, err
		}
		a.Request, a.Result, a.Evidence = json.RawMessage(req), json.RawMessage(result), json.RawMessage(evidence)
		a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		a.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- Notifications ---

func (s *Store) InsertNotification(ctx context.Context, n Notification) error {
	now := time.Now().UTC()
	if n.Status == "" {
		n.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO notifications(id,objective_id,kind,payload_json,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`,
		n.ID, n.ObjectiveID, n.Kind, jsonOr(n.Payload, "{}"), n.Status, stamp(now), stamp(now))
	return err
}
