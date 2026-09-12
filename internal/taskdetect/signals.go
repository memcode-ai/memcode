package taskdetect

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // database/sql driver

	gwconfig "github.com/memcode-ai/memcode/internal/gateway/config"
)

// Signals are the cross-session half of detection. One turn that looks
// repeatable is weak evidence; the same capability asked for in three different
// sessions, in three different words, is strong evidence — and it is only
// visible if each occasion left something durable behind.
//
// They are deliberately COMPACT. A full reflection per conversation would be
// expensive to produce and enormous to keep, and almost none of it is needed:
// the identity, a confidence, a sentence of evidence, and the proposal itself.

const schema = `
CREATE TABLE IF NOT EXISTS task_signals (
  id           TEXT PRIMARY KEY,
  session_id   TEXT NOT NULL DEFAULT '',
  observed_at  TEXT NOT NULL,
  project      TEXT NOT NULL DEFAULT '',
  family       TEXT NOT NULL,
  family_key   TEXT NOT NULL,
  fingerprint  TEXT NOT NULL,
  operation    TEXT NOT NULL DEFAULT '',
  target       TEXT NOT NULL DEFAULT '',
  scope        TEXT NOT NULL DEFAULT '',
  confidence   REAL NOT NULL DEFAULT 0,
  evidence     TEXT NOT NULL DEFAULT '',
  proposal     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS task_signals_family ON task_signals(family_key, observed_at DESC);

CREATE TABLE IF NOT EXISTS task_suppressions (
  family_key  TEXT PRIMARY KEY,
  family      TEXT NOT NULL DEFAULT '',
  scope       TEXT NOT NULL DEFAULT '',
  project     TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL
);
`

// Signal is one observation that a capability might be worth automating.
type Signal struct {
	ID          string
	SessionID   string
	ObservedAt  time.Time
	Project     string
	Family      string
	FamilyKey   string
	Fingerprint string
	Operation   string
	Target      string
	Scope       string
	Confidence  float64
	Evidence    string
	Proposal    Proposal
}

// Store is the durable signal and suppression record.
type Store struct{ db *sql.DB }

// DBPath keeps signals alongside the rest of memcode's per-machine state.
func DBPath() (string, error) {
	dir, err := gwconfig.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "signals.db"), nil
}

// Open creates or opens the signal store.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying signal schema: %w", err)
	}
	return &Store{db: db}, nil
}

// OpenDefault opens the store at its standard location.
func OpenDefault(ctx context.Context) (*Store, error) {
	p, err := DBPath()
	if err != nil {
		return nil, err
	}
	return Open(ctx, p)
}

func (s *Store) Close() error { return s.db.Close() }

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sig_" + hex.EncodeToString(b[:])
}

// Record persists one observation.
func (s *Store) Record(ctx context.Context, sessionID string, p Proposal, evidence string, now time.Time) (Signal, error) {
	blob, err := json.Marshal(p)
	if err != nil {
		return Signal{}, err
	}
	sig := Signal{
		ID: newID(), SessionID: sessionID, ObservedAt: now, Project: p.Project,
		Family: p.Family, FamilyKey: p.FamilyKey(), Fingerprint: p.Fingerprint(),
		Operation: p.Operation, Target: p.Target, Scope: p.Scope,
		Confidence: p.Confidence, Evidence: evidence, Proposal: p,
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO task_signals (id, session_id, observed_at, project, family, family_key,
		                          fingerprint, operation, target, scope, confidence, evidence, proposal)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sig.ID, sig.SessionID, now.UTC().Format(time.RFC3339Nano), sig.Project, sig.Family,
		sig.FamilyKey, sig.Fingerprint, sig.Operation, sig.Target, sig.Scope,
		sig.Confidence, evidence, string(blob))
	return sig, err
}

// Cluster is the accumulated evidence for one capability.
type Cluster struct {
	FamilyKey string
	Family    string
	Project   string
	// Weight is confidence summed with age decay. Sessions counts DISTINCT
	// sessions, which is the part that matters: three signals in one
	// conversation is one person repeating themselves, not a pattern.
	Weight   float64
	Sessions int
	Signals  int
	// Latest is the most recent proposal, used for the offer — the newest
	// phrasing of the capability is the one to show back.
	Latest   Proposal
	Evidence []string
	LastSeen time.Time
}

// Capability returns the cluster's proposal named for the CAPABILITY rather
// than for whichever instance happened to be seen last.
//
// A cluster is the recognised standing job; accepting it should produce
// "provider-catalog-maintenance", not "check-fireworks-catalog" just because
// Fireworks was the most recent thing asked about. The instance name is right
// for a single-turn offer and wrong here.
func (c Cluster) Capability() Proposal {
	p := c.Latest
	if strings.TrimSpace(c.Family) != "" {
		p.Family = c.Family
		p.Name = c.Family
	}
	return p
}

// halfLife ages evidence out. A capability someone wanted every week two years
// ago and never since is not a capability they want automated now.
const halfLife = 30 * 24 * time.Hour

func decay(age time.Duration) float64 {
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(halfLife))
}

// Clusters groups signals into capabilities, newest evidence first.
//
// Grouping is by family key, then MERGED across near-identical family names via
// SameCapability — so a classifier that named the same thing slightly
// differently on two occasions still accumulates rather than splitting.
func (s *Store) Clusters(ctx context.Context, project string, now time.Time) ([]Cluster, error) {
	q := `SELECT session_id, observed_at, project, family, family_key, confidence, evidence, proposal
	      FROM task_signals`
	var args []any
	if project != "" {
		q += ` WHERE project = ?`
		args = append(args, project)
	}
	q += ` ORDER BY observed_at DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var clusters []Cluster
	seenSessions := map[int]map[string]bool{}
	for rows.Next() {
		var sessionID, observed, proj, family, familyKey, evidence, blob string
		var conf float64
		if err := rows.Scan(&sessionID, &observed, &proj, &family, &familyKey, &conf, &evidence, &blob); err != nil {
			return nil, err
		}
		var p Proposal
		if blob != "" {
			_ = json.Unmarshal([]byte(blob), &p)
		}
		at, _ := time.Parse(time.RFC3339Nano, observed)
		w := conf * decay(now.Sub(at))

		idx := -1
		for i := range clusters {
			if clusters[i].FamilyKey == familyKey || clusters[i].Latest.SameCapability(p) {
				idx = i
				break
			}
		}
		if idx < 0 {
			clusters = append(clusters, Cluster{
				FamilyKey: familyKey, Family: family, Project: proj,
				Latest: p, LastSeen: at,
			})
			idx = len(clusters) - 1
			seenSessions[idx] = map[string]bool{}
		}
		c := &clusters[idx]
		c.Weight += w
		c.Signals++
		if evidence != "" && len(c.Evidence) < 5 {
			c.Evidence = append(c.Evidence, evidence)
		}
		if at.After(c.LastSeen) {
			c.LastSeen, c.Latest = at, p
		}
		if sessionID != "" && !seenSessions[idx][sessionID] {
			seenSessions[idx][sessionID] = true
			c.Sessions++
		}
	}
	return clusters, rows.Err()
}

// Suppress records a durable "don't suggest this kind", keyed to the capability
// rather than the wording.
//
// Declining to automate provider-catalog updates says nothing about whether to
// automate a weekly dependency audit, so suppression attaches to family, scope
// and project — never to "automation" in general.
func (s *Store) Suppress(ctx context.Context, p Proposal, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO task_suppressions (family_key, family, scope, project, created_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(family_key) DO NOTHING`,
		p.FamilyKey(), p.Family, p.Scope, p.Project, now.UTC().Format(time.RFC3339))
	return err
}

// Suppressed reports whether this capability has been refused durably.
//
// Checks the exact family key and, because a classifier's naming drifts, any
// suppressed family near enough to be the same capability. Someone who said no
// once should not be asked again because the wording moved.
func (s *Store) Suppressed(ctx context.Context, p Proposal) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT family_key, family, scope, project FROM task_suppressions`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, family, scope, project string
		if err := rows.Scan(&key, &family, &scope, &project); err != nil {
			return false, err
		}
		if key == p.FamilyKey() {
			return true, nil
		}
		other := Proposal{Family: family, Scope: scope, Project: project}
		if p.SameCapability(other) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Suppressions lists what has been refused, for `task suggestions`.
func (s *Store) Suppressions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT family, project, created_at FROM task_suppressions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var family, project, at string
		if err := rows.Scan(&family, &project, &at); err != nil {
			return nil, err
		}
		line := family
		if project != "" {
			line += " (" + project + ")"
		}
		out = append(out, line+"  "+at)
	}
	return out, rows.Err()
}

// Unsuppress lifts a durable refusal.
func (s *Store) Unsuppress(ctx context.Context, family string) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM task_suppressions WHERE family = ? OR family_key LIKE ?`,
		family, slug(family)+"|%")
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Forget drops signals for a capability, used when a task is actually created
// so its own evidence stops re-proposing it.
func (s *Store) Forget(ctx context.Context, p Proposal) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM task_signals WHERE family_key = ?`, p.FamilyKey())
	return err
}

// Prune drops evidence too old to matter, keeping the store small.
func (s *Store) Prune(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM task_signals WHERE observed_at < ?`, before.UTC().Format(time.RFC3339Nano))
	return err
}

// clip shortens evidence for storage.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
