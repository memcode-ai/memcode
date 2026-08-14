// Package state is the gateway's durable bookkeeping — the state that MUST
// survive a restart for the gateway to behave correctly: a durable INBOX of
// accepted-but-not-yet-processed messages (so a message is never lost between
// being acked to the provider and being run), and each polling channel's ack
// cursor (so a restart resumes where it left off). Both Hermes and OpenClaw's
// worst, money-losing bugs trace to keeping this state in memory; we keep it in a
// dedicated SQLite file, separate from the core event store.
//
// The inbox row's (channel, message_id) primary key also serves as the dedup
// key: a redelivery inserts nothing (fresh=false) and is dropped.
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
CREATE TABLE IF NOT EXISTS inbox (
    channel      TEXT NOT NULL,
    message_id   TEXT NOT NULL,
    conversation TEXT NOT NULL,
    principal    TEXT NOT NULL,
    text         TEXT NOT NULL,
    trusted      INTEGER NOT NULL,
    status       TEXT NOT NULL,          -- 'pending' | 'done'
    received_at  TEXT NOT NULL,
    PRIMARY KEY (channel, message_id)
);
CREATE INDEX IF NOT EXISTS idx_inbox_status ON inbox (status, received_at);

CREATE TABLE IF NOT EXISTS poll_offsets (
    channel    TEXT PRIMARY KEY,
    offset_val INTEGER NOT NULL
);
`

// Item is one inbound message durably recorded for processing.
type Item struct {
	Channel      string
	MessageID    string
	Conversation string
	Principal    string
	Text         string
	Trusted      bool
}

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

// Accept durably records an inbound message as pending and reports whether this
// call is the one that recorded it. fresh=true means "you own this message, ack
// the provider and it will be processed"; fresh=false means it was already seen
// (a duplicate delivery or a concurrent racer) and must be dropped. The insert is
// atomic, so it also guards two concurrent deliveries of the same id. Callers ack
// the provider only after Accept returns without error, so a crash before the
// durable write re-delivers rather than loses the message.
func (s *Store) Accept(ctx context.Context, it Item, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO inbox
		   (channel, message_id, conversation, principal, text, trusted, status, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		it.Channel, it.MessageID, it.Conversation, it.Principal, it.Text, boolInt(it.Trusted),
		now.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return false, fmt.Errorf("accept inbound: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// Pending returns the still-to-process items, oldest first. Used to feed the
// worker and, on startup, to replay anything a prior crash left unprocessed.
func (s *Store) Pending(ctx context.Context) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, message_id, conversation, principal, text, trusted
		   FROM inbox WHERE status = 'pending' ORDER BY received_at`)
	if err != nil {
		return nil, fmt.Errorf("pending inbox: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var trusted int
		if err := rows.Scan(&it.Channel, &it.MessageID, &it.Conversation, &it.Principal, &it.Text, &trusted); err != nil {
			return nil, err
		}
		it.Trusted = trusted != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkDone marks an item processed so it is not run again.
func (s *Store) MarkDone(ctx context.Context, channel, messageID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inbox SET status = 'done' WHERE channel = ? AND message_id = ?`, channel, messageID)
	return err
}

// PruneDone deletes processed items older than the cutoff, so the inbox can't
// grow without bound. Only 'done' rows are pruned; pending work is never dropped.
func (s *Store) PruneDone(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM inbox WHERE status = 'done' AND received_at < ?`,
		before.UTC().Format(time.RFC3339Nano))
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

// SetOffset durably records a polling channel's ack cursor.
func (s *Store) SetOffset(ctx context.Context, channel string, offset int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO poll_offsets (channel, offset_val) VALUES (?, ?)
		 ON CONFLICT(channel) DO UPDATE SET offset_val = excluded.offset_val`,
		channel, offset)
	if err != nil {
		return fmt.Errorf("set offset: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
