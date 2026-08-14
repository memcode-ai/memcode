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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
    status       TEXT NOT NULL,          -- 'pending' | 'replied' | 'done'
    reply        TEXT NOT NULL DEFAULT '', -- the job's result, held durably until delivered
    agent        TEXT NOT NULL DEFAULT '', -- persona snapshot at receipt (immutable for this task)
    project      TEXT NOT NULL DEFAULT '', -- project id snapshot at receipt (immutable for this task)
    attachments  TEXT NOT NULL DEFAULT '', -- JSON array of media spool IDs riding this message
    received_at  TEXT NOT NULL,
    PRIMARY KEY (channel, message_id)
);
CREATE INDEX IF NOT EXISTS idx_inbox_status ON inbox (status, received_at);

CREATE TABLE IF NOT EXISTS poll_offsets (
    channel    TEXT PRIMARY KEY,
    offset_val INTEGER NOT NULL
);

-- String ack cursors for channels whose resume token isn't an integer
-- (Matrix /sync since-token). Same contract as poll_offsets: advanced only
-- after a durable Deliver, so a restart resumes instead of replaying.
CREATE TABLE IF NOT EXISTS cursors (
    channel TEXT PRIMARY KEY,
    cursor  TEXT NOT NULL
);

-- Durable per-conversation selection: which persona and project this
-- conversation is currently pointed at. /agent and /project update these; a task
-- snapshots them at receipt, so changing them affects only subsequent tasks.
CREATE TABLE IF NOT EXISTS conversations (
    channel      TEXT NOT NULL,
    conversation TEXT NOT NULL,
    agent        TEXT NOT NULL DEFAULT '',
    project      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (channel, conversation)
);

-- Pending pairing requests: an unknown sender DM'd the bot and was handed a
-- one-time code. The operator approves with the code (memcode gateway pair
-- approve), which adds the principal to allow_from and deletes the row. One
-- live request per (channel, principal), so a stranger can't mint codes by
-- spamming.
CREATE TABLE IF NOT EXISTS pairings (
    code         TEXT PRIMARY KEY,
    channel      TEXT NOT NULL,
    principal    TEXT NOT NULL,
    conversation TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    UNIQUE (channel, principal)
);
`

// PairingTTL is how long a pairing code stays valid; PairingCap bounds how many
// requests may be pending at once, so a flood of strangers can't grow the table
// (or the operator's approval list) without bound.
const (
	PairingTTL = time.Hour
	PairingCap = 25
)

// Pairing is one pending request from an unknown sender.
type Pairing struct {
	Code         string
	Channel      string
	Principal    string
	Conversation string
	CreatedAt    time.Time
}

// Item is one inbound message durably recorded for processing. Reply is set only
// for items returned by PendingReplies (the job finished; the reply awaits
// delivery).
type Item struct {
	Channel      string
	MessageID    string
	Conversation string
	Principal    string
	Text         string
	Trusted      bool
	Reply        string
	Agent        string   // persona snapshot at receipt
	Project      string   // project id snapshot at receipt
	Attachments  []string // media spool IDs (bare filenames; resolved only inside the spool)
}

// Store is the gateway's durable state.
type Store struct {
	db   *sql.DB
	lock *os.File // exclusive project lock; nil on platforms without file locking
}

// Open opens (creating if needed) the gateway state DB at dir/gateway.db. It also
// takes an exclusive lock on the project so a second `memcode gateway` for the
// same repo cannot start and double-process the shared inbox — the in-memory
// dedup guard only protects a single process. The lock releases when the Store is
// closed or the process exits.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	lock, err := acquireLock(filepath.Join(dir, "gateway.lock"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "gateway.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		releaseLock(lock)
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
			releaseLock(lock)
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		releaseLock(lock)
		return nil, fmt.Errorf("applying gateway schema: %w", err)
	}
	// Bring an inbox created before these columns forward. A fresh table already
	// has them, so ignore the duplicate-column error on the older shape.
	for _, col := range []string{
		`ALTER TABLE inbox ADD COLUMN reply TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE inbox ADD COLUMN agent TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE inbox ADD COLUMN project TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE inbox ADD COLUMN attachments TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			releaseLock(lock)
			return nil, fmt.Errorf("migrating inbox: %w", err)
		}
	}
	return &Store{db: db, lock: lock}, nil
}

// OpenShared opens the gateway state DB WITHOUT the singleton lock — for CLI
// commands (pair list/approve/deny) that touch side tables while the daemon is
// running. WAL + busy_timeout make the cross-process access safe. Never use this
// to drive the inbox worker; the lock in Open exists to keep that single.
func OpenShared(ctx context.Context, dir string) (*Store, error) {
	path := filepath.Join(dir, "gateway.db")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no gateway state at %s — is the gateway set up?", path)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
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

// Close closes the database and releases the project lock.
func (s *Store) Close() error {
	err := s.db.Close()
	releaseLock(s.lock)
	return err
}

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
		   (channel, message_id, conversation, principal, text, trusted, status, agent, project, attachments, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?)`,
		it.Channel, it.MessageID, it.Conversation, it.Principal, it.Text, boolInt(it.Trusted),
		it.Agent, it.Project, encodeIDs(it.Attachments), now.UTC().Format(time.RFC3339Nano),
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
		`SELECT channel, message_id, conversation, principal, text, trusted, agent, project, attachments
		   FROM inbox WHERE status = 'pending' ORDER BY received_at`)
	if err != nil {
		return nil, fmt.Errorf("pending inbox: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var trusted int
		var atts string
		if err := rows.Scan(&it.Channel, &it.MessageID, &it.Conversation, &it.Principal, &it.Text, &trusted, &it.Agent, &it.Project, &atts); err != nil {
			return nil, err
		}
		it.Trusted = trusted != 0
		it.Attachments = decodeIDs(atts)
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetReplied durably records a finished job's reply and moves the item to
// 'replied'. From here the job is never re-run; only the reply's delivery is
// retried, so a send failure or a crash after the job completes cannot lose the
// result or repeat the work.
func (s *Store) SetReplied(ctx context.Context, channel, messageID, reply string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE inbox SET status = 'replied', reply = ? WHERE channel = ? AND message_id = ?`,
		reply, channel, messageID)
	return err
}

// PendingReplies returns items whose job finished but whose reply has not yet
// been delivered, oldest first — the outbound retry queue, drained on every tick
// and replayed after a restart.
func (s *Store) PendingReplies(ctx context.Context) ([]Item, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT channel, message_id, conversation, principal, text, trusted, reply
		   FROM inbox WHERE status = 'replied' ORDER BY received_at`)
	if err != nil {
		return nil, fmt.Errorf("pending replies: %w", err)
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		var trusted int
		if err := rows.Scan(&it.Channel, &it.MessageID, &it.Conversation, &it.Principal, &it.Text, &trusted, &it.Reply); err != nil {
			return nil, err
		}
		it.Trusted = trusted != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkDone marks an item fully processed (reply delivered) so it is not run or
// re-sent again.
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

// Cursor returns the persisted string ack cursor for a channel ("" if none).
func (s *Store) Cursor(ctx context.Context, channel string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT cursor FROM cursors WHERE channel = ?`, channel).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read cursor: %w", err)
	}
	return v, nil
}

// SetCursor durably records a channel's string ack cursor.
func (s *Store) SetCursor(ctx context.Context, channel, cursor string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cursors (channel, cursor) VALUES (?, ?)
		 ON CONFLICT(channel) DO UPDATE SET cursor = excluded.cursor`,
		channel, cursor)
	if err != nil {
		return fmt.Errorf("set cursor: %w", err)
	}
	return nil
}

// Conversation returns the persona and project this conversation currently
// points at (empty when unset — the caller applies channel/gateway defaults).
func (s *Store) Conversation(ctx context.Context, channel, conversation string) (agent, project string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT agent, project FROM conversations WHERE channel = ? AND conversation = ?`,
		channel, conversation).Scan(&agent, &project)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read conversation: %w", err)
	}
	return agent, project, nil
}

// SetConversationAgent points a conversation at a persona for its SUBSEQUENT
// tasks (upsert, preserving the current project).
func (s *Store) SetConversationAgent(ctx context.Context, channel, conversation, agent string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (channel, conversation, agent) VALUES (?, ?, ?)
		 ON CONFLICT(channel, conversation) DO UPDATE SET agent = excluded.agent`,
		channel, conversation, agent)
	return err
}

// SetConversationProject points a conversation at a project for its SUBSEQUENT
// tasks (upsert, preserving the current agent).
func (s *Store) SetConversationProject(ctx context.Context, channel, conversation, project string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO conversations (channel, conversation, project) VALUES (?, ?, ?)
		 ON CONFLICT(channel, conversation) DO UPDATE SET project = excluded.project`,
		channel, conversation, project)
	return err
}

// CreatePairing records a pending pairing request for an unknown sender and
// returns the live code. created reports whether THIS call minted it — the
// caller sends the code to the sender only then, so repeat messages from the
// same stranger don't re-trigger a reply. An expired request is replaced; a
// distinct-principal request past PairingCap is refused.
func (s *Store) CreatePairing(ctx context.Context, channel, principal, conversation, code string, now time.Time) (liveCode string, created bool, err error) {
	cutoff := now.Add(-PairingTTL).UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pairings WHERE created_at < ?`, cutoff); err != nil {
		return "", false, fmt.Errorf("pruning pairings: %w", err)
	}
	var existing string
	err = s.db.QueryRowContext(ctx,
		`SELECT code FROM pairings WHERE channel = ? AND principal = ?`, channel, principal).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, fmt.Errorf("read pairing: %w", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pairings`).Scan(&n); err != nil {
		return "", false, err
	}
	if n >= PairingCap {
		return "", false, fmt.Errorf("too many pending pairing requests (%d)", n)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pairings (code, channel, principal, conversation, created_at) VALUES (?, ?, ?, ?, ?)`,
		code, channel, principal, conversation, now.UTC().Format(time.RFC3339Nano)); err != nil {
		return "", false, fmt.Errorf("record pairing: %w", err)
	}
	return code, true, nil
}

// PendingPairings lists live (non-expired) pairing requests, oldest first.
func (s *Store) PendingPairings(ctx context.Context, now time.Time) ([]Pairing, error) {
	cutoff := now.Add(-PairingTTL).UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx,
		`SELECT code, channel, principal, conversation, created_at FROM pairings
		  WHERE created_at >= ? ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("pending pairings: %w", err)
	}
	defer rows.Close()
	var out []Pairing
	for rows.Next() {
		var p Pairing
		var created string
		if err := rows.Scan(&p.Code, &p.Channel, &p.Principal, &p.Conversation, &created); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// TakePairing removes a pairing request by code (case-insensitive) and returns
// it — the approve/deny consume step. A missing or expired code is an error.
func (s *Store) TakePairing(ctx context.Context, code string, now time.Time) (Pairing, error) {
	var p Pairing
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT code, channel, principal, conversation, created_at FROM pairings WHERE code = ?`,
		strings.ToUpper(strings.TrimSpace(code))).Scan(&p.Code, &p.Channel, &p.Principal, &p.Conversation, &created)
	if err == sql.ErrNoRows {
		return Pairing{}, fmt.Errorf("no pairing request with code %q", code)
	}
	if err != nil {
		return Pairing{}, fmt.Errorf("read pairing: %w", err)
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pairings WHERE code = ?`, p.Code); err != nil {
		return Pairing{}, fmt.Errorf("consume pairing: %w", err)
	}
	if now.Sub(p.CreatedAt) > PairingTTL {
		return Pairing{}, fmt.Errorf("pairing code %s expired — have them message the bot again", p.Code)
	}
	return p, nil
}

// encodeIDs/decodeIDs carry the media spool IDs through the inbox row as JSON.
// IDs are bare spool filenames — never paths; the consumer resolves them only
// inside the spool directory.
func encodeIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeIDs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var ids []string
	if json.Unmarshal([]byte(s), &ids) != nil {
		return nil
	}
	return ids
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
