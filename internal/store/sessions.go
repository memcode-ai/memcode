package store

import (
	"context"
	"fmt"
	"time"
)

// Per-session rows. The events table has no session column: the runtime stamps
// session_id into every agent event's payload, so these read it with
// json_extract. Both take a set of kinds to leave alone — the event kinds that
// feed persistent memory (preference and lesson signals, adherence verdicts,
// the user's notes and decisions), which outlive the session that produced them.

// sessionIDExpr extracts an event's session id from its payload.
const sessionIDExpr = `json_extract(payload_json, '$.session_id')`

// SessionActivity returns every session id that has events of a kind outside
// skipKinds, mapped to its newest event time.
func (s *sqliteStore) SessionActivity(ctx context.Context, skipKinds []string) (map[string]time.Time, error) {
	q := `SELECT ` + sessionIDExpr + ` AS sid, MAX(ts) FROM events WHERE json_valid(payload_json) AND ` +
		sessionIDExpr + ` IS NOT NULL`
	var args []any
	if len(skipKinds) > 0 {
		q += ` AND kind NOT IN (` + placeholders(len(skipKinds)) + `)`
		for _, k := range skipKinds {
			args = append(args, k)
		}
	}
	q += ` GROUP BY sid`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("session activity: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var sid, ts string
		if err := rows.Scan(&sid, &ts); err != nil {
			return nil, err
		}
		if sid != "" {
			out[sid] = parseTime(ts)
		}
	}
	return out, rows.Err()
}

// DeleteSessionEvents removes the events of the given sessions, except those of
// a kind in keepKinds, and reports how many rows went.
func (s *sqliteStore) DeleteSessionEvents(ctx context.Context, sessionIDs, keepKinds []string) (int64, error) {
	if len(sessionIDs) == 0 {
		return 0, nil
	}
	var total int64
	// Batches keep the statement under SQLite's bound-parameter limit.
	const batch = 200
	for start := 0; start < len(sessionIDs); start += batch {
		ids := sessionIDs[start:min(start+batch, len(sessionIDs))]
		q := `DELETE FROM events WHERE json_valid(payload_json) AND ` + sessionIDExpr +
			` IN (` + placeholders(len(ids)) + `)`
		args := make([]any, 0, len(ids)+len(keepKinds))
		for _, id := range ids {
			args = append(args, id)
		}
		if len(keepKinds) > 0 {
			q += ` AND kind NOT IN (` + placeholders(len(keepKinds)) + `)`
			for _, k := range keepKinds {
				args = append(args, k)
			}
		}
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("delete session events: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
