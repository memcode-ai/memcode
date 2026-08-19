// Package store is the persistence layer for the memcode state engine. The
// event log is the source of truth; entities, edges, objectives and
// current_state are materialized projections. Everything sits behind the Store
// interface so the local SQLite backend can be swapped for a cloud backend
// (e.g. Postgres) later without touching callers.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (keeps CGO_ENABLED=0)
)

//go:embed schema.sql
var schema string

// Event is one row in the append-only log — the source of truth.
type Event struct {
	ID        int64
	TS        time.Time
	Kind      string
	Actor     string
	Payload   json.RawMessage
	ValidFrom *time.Time
	ValidTo   *time.Time
}

// Entity is a node in the top-down model (subsystem, concept, doctrine, …).
type Entity struct {
	ID        string // "<kind>:<key>"
	Kind      string
	Key       string
	Attrs     json.RawMessage
	ValidFrom *time.Time
	ValidTo   *time.Time
}

// Edge is a typed relationship between two entities.
type Edge struct {
	Src       string
	Dst       string
	Kind      string
	Attrs     json.RawMessage
	ValidFrom *time.Time
	ValidTo   *time.Time
}

// Objective is a human-authored goal.
type Objective struct {
	ID        string
	Title     string
	Status    string
	Priority  int
	ParentID  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// State is one materialized current_state layer for a scope.
type State struct {
	Scope         string
	Layer         string
	Body          json.RawMessage
	SourceEventID *int64
	RefreshedAt   time.Time
}

// Claim is an adjudicated assertion extracted from sources + evidence.
type Claim struct {
	ID               string
	Type             string // doctrine | preference | command | decision | warning
	Text             string
	Scope            string
	Status           string // candidate | current | stale | conflicted | rejected
	Confidence       string
	Evidence         string
	SourcePath       string
	SourceModifiedAt string
	ExtractedAt      time.Time
}

// PreferenceCandidate is a behavior-derived directive that accumulated enough
// weighted evidence to be a standing preference candidate. Materialized from
// preference_signal events by the prefs reducer. The event log is the source of
// truth; this table is a projection rebuilt atomically by prefs.Reduce.
type PreferenceCandidate struct {
	ID            string
	Axis          string
	Text          string
	Scope         string
	Weight        float64
	SignalCount   int
	SessionCount  int
	FirstSeen     time.Time
	LastSeen      time.Time
	Status        string // candidate | confirmed | demoted
	ConfirmedPath string
	Evidence      json.RawMessage
}

// EventFilter narrows ListEvents. Zero value matches everything.
type EventFilter struct {
	Kinds []string // any-of; empty = all kinds
	Limit int      // 0 = no limit
}

// EdgeFilter narrows ListEdges. Empty fields are wildcards.
type EdgeFilter struct {
	Src  string
	Dst  string
	Kind string
}

// Store is the persistence contract for the engine.
type Store interface {
	AppendEvent(ctx context.Context, e Event) (int64, error)
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)

	UpsertEntity(ctx context.Context, e Entity) error
	ListEntities(ctx context.Context, kind string) ([]Entity, error) // kind="" = all
	GetEntity(ctx context.Context, id string) (Entity, bool, error)

	UpsertEdge(ctx context.Context, e Edge) error
	ListEdges(ctx context.Context, f EdgeFilter) ([]Edge, error)

	CreateObjective(ctx context.Context, o Objective) error
	UpdateObjective(ctx context.Context, o Objective) error
	GetObjective(ctx context.Context, id string) (Objective, bool, error)
	ListObjectives(ctx context.Context) ([]Objective, error)

	PutState(ctx context.Context, s State) error
	GetState(ctx context.Context, scope, layer string) (State, bool, error)

	AddClaim(ctx context.Context, c Claim) error
	ListClaims(ctx context.Context) ([]Claim, error)
	ClearClaims(ctx context.Context) error

	AddPreferenceCandidate(ctx context.Context, c PreferenceCandidate) error
	ListPreferenceCandidates(ctx context.Context) ([]PreferenceCandidate, error)
	ClearPreferenceCandidates(ctx context.Context) error
	UpdatePreferenceCandidateStatus(ctx context.Context, id, status, confirmedPath string, weight float64) error

	// RunInTx runs fn inside a single database transaction. fn receives a Tx that
	// exposes the claim-mutation methods (and may grow more as needed). The
	// transaction commits when fn returns nil and rolls back on any error. This is
	// the atomicity seam for operations that must not leave a partial state — e.g.
	// learn.Run's clear+re-insert claim rebuild. Additive: callers that don't use
	// it are unaffected.
	RunInTx(ctx context.Context, fn func(Tx) error) error

	Close() error
}

// Tx is a transaction-scoped handle to the claim-mutation methods. It exists so a
// caller can make multiple writes atomically (e.g. ClearClaims + a batch of
// AddClaim) without exposing the raw *sql.Tx. Methods mirror their Store
// counterparts — same SQL, same semantics — just executed against the transaction.
type Tx interface {
	ClearClaims(ctx context.Context) error
	AddClaim(ctx context.Context, c Claim) error
	ClearPreferenceCandidates(ctx context.Context) error
	AddPreferenceCandidate(ctx context.Context, c PreferenceCandidate) error
}

type sqliteStore struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. The returned Store is safe for sequential use by a single CLI process.
func Open(ctx context.Context, path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	// Multiple memcode processes may open the same DB (background jobs, parallel
	// explorers). busy_timeout MUST be set first so the journal_mode=WAL switch —
	// which needs a brief exclusive lock — waits for a concurrent opener instead
	// of failing immediately with "database is locked".
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

// migrations is the ordered schema history: migrations[i] brings a database to
// version i+1. Version 1 is the current base schema (all CREATE ... IF NOT
// EXISTS, so pre-versioning databases — user_version 0 with the tables already
// present — replay it harmlessly and get stamped). Append future changes here;
// never edit or reorder shipped entries.
var migrations = []string{schema}

// migrate applies every migration past the database's PRAGMA user_version and
// stamps the new version after each step, so a partial failure resumes cleanly.
func migrate(ctx context.Context, db *sql.DB) error {
	var v int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d) — upgrade memcode", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := db.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("applying schema migration to version %d: %w", i+1, err)
		}
		// PRAGMA doesn't take placeholders; i is a trusted loop index.
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return fmt.Errorf("stamping schema version %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func (s *sqliteStore) AppendEvent(ctx context.Context, e Event) (int64, error) {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, kind, actor, payload_json, valid_from, valid_to)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rfc(&e.TS), e.Kind, nullStr(e.Actor), nullJSON(e.Payload),
		rfc(e.ValidFrom), rfc(e.ValidTo),
	)
	if err != nil {
		return 0, fmt.Errorf("append event: %w", err)
	}
	return res.LastInsertId()
}

func (s *sqliteStore) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	where := ""
	var args []any
	if len(f.Kinds) > 0 {
		where = " WHERE kind IN (" + placeholders(len(f.Kinds)) + ")"
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	// A LIMIT must return the MOST RECENT N in chronological order, not the oldest N.
	// Previously `ORDER BY id ASC LIMIT N` returned the FIRST-ever N events, so a capped
	// scan (e.g. the prefs reducer's 5000-signal bound) froze on ancient history and
	// never saw new events. Select the newest N (id DESC) then re-order ASC.
	q := `SELECT id, ts, kind, actor, payload_json, valid_from, valid_to FROM events`
	if f.Limit > 0 {
		q = `SELECT id, ts, kind, actor, payload_json, valid_from, valid_to FROM (` +
			q + where + ` ORDER BY id DESC LIMIT ?) ORDER BY id ASC`
		args = append(args, f.Limit)
	} else {
		q += where + " ORDER BY id ASC"
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e                          Event
			ts                         string
			actor, payload, vfrom, vto sql.NullString
		)
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &actor, &payload, &vfrom, &vto); err != nil {
			return nil, err
		}
		e.TS = parseTime(ts)
		e.Actor = actor.String
		e.Payload = rawJSON(payload)
		e.ValidFrom = parseNullTime(vfrom)
		e.ValidTo = parseNullTime(vto)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) UpsertEntity(ctx context.Context, e Entity) error {
	if e.ID == "" {
		e.ID = e.Kind + ":" + e.Key
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, kind, key, attrs_json, valid_from, valid_to)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   attrs_json = excluded.attrs_json,
		   valid_from = excluded.valid_from,
		   valid_to   = excluded.valid_to`,
		e.ID, e.Kind, e.Key, nullJSON(e.Attrs), rfc(e.ValidFrom), rfc(e.ValidTo),
	)
	if err != nil {
		return fmt.Errorf("upsert entity %s: %w", e.ID, err)
	}
	return nil
}

func (s *sqliteStore) ListEntities(ctx context.Context, kind string) ([]Entity, error) {
	q := `SELECT id, kind, key, attrs_json, valid_from, valid_to FROM entities`
	var args []any
	if kind != "" {
		q += " WHERE kind = ?"
		args = append(args, kind)
	}
	q += " ORDER BY key ASC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetEntity(ctx context.Context, id string) (Entity, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, key, attrs_json, valid_from, valid_to FROM entities WHERE id = ?`, id)
	if err != nil {
		return Entity{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Entity{}, false, rows.Err()
	}
	e, err := scanEntity(rows)
	return e, err == nil, err
}

func (s *sqliteStore) UpsertEdge(ctx context.Context, e Edge) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO edges (src_id, dst_id, kind, attrs_json, valid_from, valid_to)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(src_id, dst_id, kind) DO UPDATE SET
		   attrs_json = excluded.attrs_json,
		   valid_from = excluded.valid_from,
		   valid_to   = excluded.valid_to`,
		e.Src, e.Dst, e.Kind, nullJSON(e.Attrs), rfc(e.ValidFrom), rfc(e.ValidTo),
	)
	if err != nil {
		return fmt.Errorf("upsert edge %s-[%s]->%s: %w", e.Src, e.Kind, e.Dst, err)
	}
	return nil
}

func (s *sqliteStore) ListEdges(ctx context.Context, f EdgeFilter) ([]Edge, error) {
	q := `SELECT src_id, dst_id, kind, attrs_json, valid_from, valid_to FROM edges WHERE 1=1`
	var args []any
	if f.Src != "" {
		q += " AND src_id = ?"
		args = append(args, f.Src)
	}
	if f.Dst != "" {
		q += " AND dst_id = ?"
		args = append(args, f.Dst)
	}
	if f.Kind != "" {
		q += " AND kind = ?"
		args = append(args, f.Kind)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Edge
	for rows.Next() {
		var (
			e                 Edge
			attrs, vfrom, vto sql.NullString
		)
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &attrs, &vfrom, &vto); err != nil {
			return nil, err
		}
		e.Attrs = rawJSON(attrs)
		e.ValidFrom = parseNullTime(vfrom)
		e.ValidTo = parseNullTime(vto)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) CreateObjective(ctx context.Context, o Objective) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO objectives (id, title, status, priority, parent_objective_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		o.ID, o.Title, o.Status, o.Priority, nullStr(o.ParentID),
		rfc(&o.CreatedAt), rfc(&o.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("create objective: %w", err)
	}
	return nil
}

func (s *sqliteStore) UpdateObjective(ctx context.Context, o Objective) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE objectives SET title=?, status=?, priority=?, parent_objective_id=?, updated_at=?
		 WHERE id=?`,
		o.Title, o.Status, o.Priority, nullStr(o.ParentID), rfc(&o.UpdatedAt), o.ID,
	)
	if err != nil {
		return fmt.Errorf("update objective %s: %w", o.ID, err)
	}
	// A WHERE id=? that matches nothing is a no-op the caller almost never wants silently —
	// surface "no such objective" instead of reporting success on a write that didn't land.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("update objective %s: no such objective", o.ID)
	}
	return nil
}

func (s *sqliteStore) GetObjective(ctx context.Context, id string) (Objective, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, status, priority, parent_objective_id, created_at, updated_at
		 FROM objectives WHERE id = ?`, id)
	if err != nil {
		return Objective{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Objective{}, false, rows.Err()
	}
	o, err := scanObjective(rows)
	return o, err == nil, err
}

func (s *sqliteStore) ListObjectives(ctx context.Context) ([]Objective, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, status, priority, parent_objective_id, created_at, updated_at
		 FROM objectives ORDER BY priority DESC, created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Objective
	for rows.Next() {
		o, err := scanObjective(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PutState(ctx context.Context, st State) error {
	if st.RefreshedAt.IsZero() {
		st.RefreshedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO current_state (scope, layer, body_json, source_event_id, refreshed_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(scope, layer) DO UPDATE SET
		   body_json       = excluded.body_json,
		   source_event_id = excluded.source_event_id,
		   refreshed_at    = excluded.refreshed_at`,
		st.Scope, st.Layer, nullJSON(st.Body), nullInt(st.SourceEventID), rfc(&st.RefreshedAt),
	)
	if err != nil {
		return fmt.Errorf("put state %s/%s: %w", st.Scope, st.Layer, err)
	}
	return nil
}

func (s *sqliteStore) GetState(ctx context.Context, scope, layer string) (State, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT scope, layer, body_json, source_event_id, refreshed_at
		 FROM current_state WHERE scope = ? AND layer = ?`, scope, layer)
	var (
		st      State
		body    sql.NullString
		srcID   sql.NullInt64
		refresh sql.NullString
	)
	switch err := row.Scan(&st.Scope, &st.Layer, &body, &srcID, &refresh); err {
	case sql.ErrNoRows:
		return State{}, false, nil
	case nil:
		st.Body = rawJSON(body)
		if srcID.Valid {
			st.SourceEventID = &srcID.Int64
		}
		st.RefreshedAt = parseTime(refresh.String)
		return st, true, nil
	default:
		return State{}, false, err
	}
}

func (s *sqliteStore) AddClaim(ctx context.Context, c Claim) error {
	if c.ExtractedAt.IsZero() {
		c.ExtractedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO claims (id, claim_type, text, scope, status, confidence, evidence, source_path, source_modified_at, extracted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Type, c.Text, nullStr(c.Scope), c.Status, nullStr(c.Confidence),
		nullStr(c.Evidence), nullStr(c.SourcePath), nullStr(c.SourceModifiedAt), rfc(&c.ExtractedAt),
	)
	if err != nil {
		return fmt.Errorf("add claim: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListClaims(ctx context.Context) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, claim_type, text, scope, status, confidence, evidence, source_path, source_modified_at, extracted_at
		 FROM claims ORDER BY status, claim_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var (
			c                                       Claim
			scope, conf, ev, src, srcMod, extracted sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Type, &c.Text, &scope, &c.Status, &conf, &ev, &src, &srcMod, &extracted); err != nil {
			return nil, err
		}
		c.Scope = scope.String
		c.Confidence = conf.String
		c.Evidence = ev.String
		c.SourcePath = src.String
		c.SourceModifiedAt = srcMod.String
		c.ExtractedAt = parseTime(extracted.String)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ClearClaims(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM claims`)
	return err
}

func (s *sqliteStore) AddPreferenceCandidate(ctx context.Context, c PreferenceCandidate) error {
	if c.Scope == "" {
		c.Scope = "."
	}
	if c.Status == "" {
		c.Status = "candidate"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO preference_candidates
		   (id, axis, text, scope, weight, signal_count, session_count, first_seen, last_seen, status, confirmed_path, evidence_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Axis, c.Text, c.Scope, c.Weight, c.SignalCount, c.SessionCount,
		rfc(&c.FirstSeen), rfc(&c.LastSeen), c.Status, nullStr(c.ConfirmedPath), nullJSON(c.Evidence),
	)
	if err != nil {
		return fmt.Errorf("add preference candidate: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListPreferenceCandidates(ctx context.Context) ([]PreferenceCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, axis, text, scope, weight, signal_count, session_count, first_seen, last_seen, status, confirmed_path, evidence_json
		 FROM preference_candidates ORDER BY status, weight DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PreferenceCandidate
	for rows.Next() {
		var (
			c                       PreferenceCandidate
			first, last             sql.NullString
			confirmedPath, evidence sql.NullString
		)
		if err := rows.Scan(&c.ID, &c.Axis, &c.Text, &c.Scope, &c.Weight, &c.SignalCount, &c.SessionCount,
			&first, &last, &c.Status, &confirmedPath, &evidence); err != nil {
			return nil, err
		}
		c.FirstSeen = parseTime(first.String)
		c.LastSeen = parseTime(last.String)
		c.ConfirmedPath = confirmedPath.String
		c.Evidence = rawJSON(evidence)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ClearPreferenceCandidates(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM preference_candidates`)
	return err
}

func (s *sqliteStore) UpdatePreferenceCandidateStatus(ctx context.Context, id, status, confirmedPath string, weight float64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE preference_candidates SET status=?, confirmed_path=?, weight=? WHERE id=?`,
		status, nullStr(confirmedPath), weight, id,
	)
	if err != nil {
		return fmt.Errorf("update preference candidate %s: %w", id, err)
	}
	// Same contract as UpdateObjective: a WHERE id=? that matches nothing must not
	// report success on a write that didn't land.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("update preference candidate %s: no such candidate", id)
	}
	return nil
}

// --- transaction support ---

// claimTx is a Tx backed by a *sql.Tx. Its methods mirror the sqliteStore claim
// methods — same SQL, same parameterization — just executed against the transaction
// so a multi-step claim rebuild (ClearClaims + a batch of AddClaim) commits atomically.
type claimTx struct {
	tx *sql.Tx
}

func (t *claimTx) ClearClaims(ctx context.Context) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM claims`)
	return err
}

func (t *claimTx) AddClaim(ctx context.Context, c Claim) error {
	if c.ExtractedAt.IsZero() {
		c.ExtractedAt = time.Now().UTC()
	}
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO claims (id, claim_type, text, scope, status, confidence, evidence, source_path, source_modified_at, extracted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Type, c.Text, nullStr(c.Scope), c.Status, nullStr(c.Confidence),
		nullStr(c.Evidence), nullStr(c.SourcePath), nullStr(c.SourceModifiedAt), rfc(&c.ExtractedAt),
	)
	if err != nil {
		return fmt.Errorf("add claim: %w", err)
	}
	return nil
}

func (t *claimTx) ClearPreferenceCandidates(ctx context.Context) error {
	_, err := t.tx.ExecContext(ctx, `DELETE FROM preference_candidates`)
	return err
}

func (t *claimTx) AddPreferenceCandidate(ctx context.Context, c PreferenceCandidate) error {
	if c.Scope == "" {
		c.Scope = "."
	}
	if c.Status == "" {
		c.Status = "candidate"
	}
	_, err := t.tx.ExecContext(ctx,
		`INSERT INTO preference_candidates
		   (id, axis, text, scope, weight, signal_count, session_count, first_seen, last_seen, status, confirmed_path, evidence_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Axis, c.Text, c.Scope, c.Weight, c.SignalCount, c.SessionCount,
		rfc(&c.FirstSeen), rfc(&c.LastSeen), c.Status, nullStr(c.ConfirmedPath), nullJSON(c.Evidence),
	)
	if err != nil {
		return fmt.Errorf("add preference candidate: %w", err)
	}
	return nil
}

// RunInTx begins a transaction, wraps it in a claimTx, calls fn, and commits on nil
// or rolls back on error. A panic in fn also rolls back (deferred rollback runs before
// the panic propagates). This is the atomicity seam: a mid-loop failure in learn.Run's
// claim rebuild leaves the old claim set intact instead of a partial new set.
func (s *sqliteStore) RunInTx(ctx context.Context, fn func(Tx) error) error {
	dbtx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Defer rollback: if commit succeeds, this is a no-op (Rollback on a committed tx
	// returns sql.ErrTxDone, which we ignore). If fn returns an error or panics, the
	// rollback discards all writes.
	defer func() { _ = dbtx.Rollback() }()
	if err := fn(&claimTx{tx: dbtx}); err != nil {
		return err
	}
	if err := dbtx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// --- scan helpers ---

func scanEntity(rows *sql.Rows) (Entity, error) {
	var (
		e                 Entity
		attrs, vfrom, vto sql.NullString
	)
	if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &attrs, &vfrom, &vto); err != nil {
		return Entity{}, err
	}
	e.Attrs = rawJSON(attrs)
	e.ValidFrom = parseNullTime(vfrom)
	e.ValidTo = parseNullTime(vto)
	return e, nil
}

func scanObjective(rows *sql.Rows) (Objective, error) {
	var (
		o       Objective
		parent  sql.NullString
		created string
		updated string
	)
	if err := rows.Scan(&o.ID, &o.Title, &o.Status, &o.Priority, &parent, &created, &updated); err != nil {
		return Objective{}, err
	}
	o.ParentID = parent.String
	o.CreatedAt = parseTime(created)
	o.UpdatedAt = parseTime(updated)
	return o, nil
}
