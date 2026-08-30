package personal

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Interaction is a durable human-in-the-loop request created by a suspending
// tool (ask_user). It lives in the agent's personal.db and is answered via
// `personal answer`. Resume is exact: the saved tool_use_id gets the answer.
type Interaction struct {
	ID, AgentID, ObjectiveID, RunID string
	Kind, Question, Context         string
	Answer                          *string
	Status                          string // pending | answered | cancelled
	ToolUseID                       string
	CreatedAt                       time.Time
	AnsweredAt                      *time.Time
}

func (s *Store) InsertInteraction(ctx context.Context, in Interaction) error {
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now().UTC()
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO interactions(id,agent_id,objective_id,run_id,kind,question,context,status,tool_use_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		in.ID, in.AgentID, in.ObjectiveID, in.RunID, in.Kind, in.Question, in.Context, in.Status, in.ToolUseID, stamp(in.CreatedAt))
	return err
}

func scanInteraction(row interface{ Scan(...any) error }) (Interaction, error) {
	var in Interaction
	var answer, answered sql.NullString
	var created string
	err := row.Scan(&in.ID, &in.AgentID, &in.ObjectiveID, &in.RunID, &in.Kind, &in.Question, &in.Context, &answer, &in.Status, &in.ToolUseID, &created, &answered)
	if err != nil {
		return in, err
	}
	in.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if answer.Valid {
		in.Answer = &answer.String
	}
	if answered.Valid {
		t, _ := time.Parse(time.RFC3339Nano, answered.String)
		in.AnsweredAt = &t
	}
	return in, nil
}

const interactionCols = `id,agent_id,objective_id,run_id,kind,question,context,answer,status,tool_use_id,created_at,answered_at`

func (s *Store) PendingInteractions(ctx context.Context, agentID string) ([]Interaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+interactionCols+` FROM interactions WHERE agent_id=? AND status='pending' ORDER BY created_at`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Interaction
	for rows.Next() {
		in, err := scanInteraction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) GetInteraction(ctx context.Context, id string) (Interaction, bool, error) {
	in, err := scanInteraction(s.db.QueryRowContext(ctx, `SELECT `+interactionCols+` FROM interactions WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return Interaction{}, false, nil
	}
	if err != nil {
		return Interaction{}, false, err
	}
	return in, true, nil
}

// ResolveInteraction atomically marks a pending interaction answered. Returns an
// error if it was already resolved (prevents double-resume of a suspended run).
func (s *Store) ResolveInteraction(ctx context.Context, id, answer string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE interactions SET status='answered',answer=?,answered_at=? WHERE id=? AND status='pending'`, answer, stamp(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("interaction %q is not pending (already answered or cancelled)", id)
	}
	return nil
}

func (s *Store) CancelInteraction(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE interactions SET status='cancelled' WHERE id=? AND status='pending'`, id)
	return err
}

// Package-level wrappers used by cmd (store passed explicitly).
func PendingInteractions(s *Store, agentID string) ([]Interaction, error) {
	return s.PendingInteractions(context.Background(), agentID)
}
func GetInteraction(s *Store, id string) (Interaction, bool, error) {
	return s.GetInteraction(context.Background(), id)
}
func ResolveInteraction(s *Store, id, answer string) error {
	return s.ResolveInteraction(context.Background(), id, answer)
}
