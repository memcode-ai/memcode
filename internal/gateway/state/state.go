// Package state is the gateway's durable bookkeeping — the small amount of state
// that MUST survive a restart for the gateway to behave correctly: which inbound
// messages have already been dispatched (so a restart or reconnect never re-runs
// an old message as a fresh, paid agent turn), and each polling channel's ack
// cursor (so a restart resumes exactly where it left off). Both Hermes and
// OpenClaw's worst, money-losing bugs trace to keeping this state in memory; we
// keep it in a dedicated SQLite file, separate from the core event store so the
// spine's interface stays clean.
package state

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS processed_messages (
    channel    TEXT NOT NULL,
    message_id TEXT NOT NULL,
    seen_at    TEXT NOT NULL,
    PRIMARY KEY (channel, message_id)
);
CREATE INDEX IF NOT EXISTS idx_processed_seen_at ON processed_messages (seen_at);

CREATE TABLE IF NOT EXISTS poll_offsets (
    channel    TEXT PRIMARY KEY,
    offset_val INTEGER NOT NULL
);
`

// Store is the gateway's durable state.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the gateway state DB at dir/gateway.db.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	path := filepath.Join(dir, "gateway.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// busy_timeout first, then WAL — the gateway and detached agent jobs may touch
	// the project concurrently, so the WAL switch must wait for a lock, not fail.
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying gateway schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// MarkProcessed atomically records that (channel, messageID) has been dispatched
// and reports whether this call is the one that recorded it. fresh=true means
// "you own this message, dispatch it"; fresh=false means it was already seen (a
// duplicate delivery or a concurrent racer) and must be dropped. The insert is
// atomic, so it doubles as the in-flight guard: of two concurrent deliveries of
// the same id, exactly one gets fresh=true.
func (s *Store) MarkProcessed(ctx context.Context, channel, messageID string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_messages (channel, message_id, seen_at) VALUES (?, ?, ?)`,
		channel, messageID, now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("mark processed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// PruneProcessed deletes processed-message records older than the cutoff, so the
// dedup table can't grow without bound. Duplicate deliveries only ever arrive
// close in time to the original, so an old record is safe to forget.
func (s *Store) PruneProcessed(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM processed_messages WHERE seen_at < ?`,
		before.UTC().Format(time.RFC3339Nano),
	)
	return err
}

// Offset returns the persisted ack cursor for a polling channel, or 0 if none.
func (s *Store) Offset(ctx context.Context, channel string) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT offset_val FROM poll_offsets WHERE channel = ?`, channel).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read offset: %w", err)
	}
	return v, nil
}

// SetOffset durably records a polling channel's ack cursor. Callers persist the
// cursor for an update only after it has been dispatched, so a crash re-delivers
// rather than skips.
func (s *Store) SetOffset(ctx context.Context, channel string, offset int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_offsets (channel, offset_val) VALUES (?, ?)
		 ON CONFLICT(channel) DO UPDATE SET offset_val = excluded.offset_val`,
		channel, offset,
	)
	if err != nil {
		return fmt.Errorf("set offset: %w", err)
	}
	return nil
}
