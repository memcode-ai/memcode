package autonomy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ActionStatus string

const (
	ActionPlanned   ActionStatus = "planned"
	ActionReserved  ActionStatus = "reserved"
	ActionRunning   ActionStatus = "running"
	ActionSucceeded ActionStatus = "succeeded"
	ActionFailed    ActionStatus = "failed"
	ActionUncertain ActionStatus = "uncertain"
	ActionCancelled ActionStatus = "cancelled"
)

type ActionIntent struct {
	ID, ObjectiveID, SubgoalID, RunID, Kind, Target string
	Consequence                                     ConsequenceClass
	PolicyHash                                      string
	Request                                         json.RawMessage
	IdempotencyKey                                  string
}

func RedactActionRequest(v json.RawMessage) json.RawMessage {
	var x any
	if json.Unmarshal(v, &x) != nil {
		return json.RawMessage(`"[redacted]"`)
	}
	redactValue(x)
	b, _ := json.Marshal(x)
	return b
}
func redactValue(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, val := range m {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "cookie") || strings.Contains(lower, "authorization") {
			m[k] = "[redacted]"
		} else {
			redactValue(val)
		}
	}
}
func (s *Store) ReserveAction(ctx context.Context, a ActionIntent) (Action, bool, error) {
	now := time.Now().UTC()
	request := RedactActionRequest(a.Request)
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO actions(id,objective_id,subgoal_id,run_id,kind,target,consequence_class,policy_hash,request_json,idempotency_key,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.ID, a.ObjectiveID, a.SubgoalID, a.RunID, a.Kind, a.Target, a.Consequence, a.PolicyHash, string(request), nullableString(a.IdempotencyKey), ActionReserved, stamp(now), stamp(now))
	if err != nil {
		return Action{}, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 && a.IdempotencyKey != "" {
		var existing Action
		err = s.db.QueryRowContext(ctx, `SELECT id,status FROM actions WHERE objective_id=? AND idempotency_key=?`, a.ObjectiveID, a.IdempotencyKey).Scan(&existing.ID, &existing.Status)
		return existing, false, err
	}
	return Action{ID: a.ID, ObjectiveID: a.ObjectiveID, Status: string(ActionReserved)}, n == 1, nil
}
func (s *Store) CompleteAction(ctx context.Context, id string, status ActionStatus, result, evidence json.RawMessage) error {
	if status != ActionSucceeded && status != ActionFailed && status != ActionUncertain && status != ActionCancelled {
		return fmt.Errorf("invalid terminal action status %q", status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,result_json=?,evidence_json=?,updated_at=? WHERE id=? AND status IN ('reserved','running')`, status, string(result), string(evidence), stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("action %q is not reservable/running", id)
	}
	return nil
}

// LinkActionJob records which detached job an action spawned, so a later wake
// can find its way back from a job id to the action it must close out.
func (s *Store) LinkActionJob(ctx context.Context, actionID, jobID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE actions SET job_id=?,updated_at=? WHERE id=?`, jobID, stamp(time.Now().UTC()), actionID)
	return err
}

// ActionForJob returns the id of the action that spawned jobID, or "" when
// there is none.
func (s *Store) ActionForJob(ctx context.Context, jobID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM actions WHERE job_id=?`, jobID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

func (s *Store) MarkActionRunning(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET status='running',updated_at=? WHERE id=? AND status='reserved'`, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
