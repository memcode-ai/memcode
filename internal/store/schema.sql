-- memcode state schema. The event log is the source of truth; everything else is
-- a materialized projection that can be dropped and rebuilt by replaying events.

CREATE TABLE IF NOT EXISTS events (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            TEXT NOT NULL,            -- RFC3339
    kind          TEXT NOT NULL,           -- commit | agent_action | user_note | assertion | decision | frustration | ...
    actor         TEXT,                    -- who/what produced it
    payload_json  TEXT,                    -- arbitrary JSON
    valid_from    TEXT,                    -- when the asserted fact became true
    valid_to      TEXT                     -- when it stopped being true (NULL = still true)
);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events (kind);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events (ts);

-- Goals: what we're TRYING to do. Human-authored in v1 (no auto-inference).
CREATE TABLE IF NOT EXISTS objectives (
    id                  TEXT PRIMARY KEY,
    title               TEXT NOT NULL,
    status              TEXT NOT NULL,      -- proposed | active | blocked | done | abandoned
    priority            INTEGER NOT NULL DEFAULT 0,
    parent_objective_id TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

-- Top-down entities. file/symbol are drill-down only (populated later).
CREATE TABLE IF NOT EXISTS entities (
    id          TEXT PRIMARY KEY,          -- "<kind>:<key>"
    kind        TEXT NOT NULL,             -- subsystem | concept | boundary | doctrine | decision | file | symbol
    key         TEXT NOT NULL,
    attrs_json  TEXT,
    valid_from  TEXT,
    valid_to    TEXT,
    UNIQUE (kind, key)
);
CREATE INDEX IF NOT EXISTS idx_entities_kind ON entities (kind);

CREATE TABLE IF NOT EXISTS edges (
    src_id      TEXT NOT NULL,
    dst_id      TEXT NOT NULL,
    kind        TEXT NOT NULL,             -- depends_on | belongs_to | implements | constrained_by | serves | blocks
    attrs_json  TEXT,
    valid_from  TEXT,
    valid_to    TEXT,
    PRIMARY KEY (src_id, dst_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_edges_src ON edges (src_id);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges (dst_id);

-- (Remembered command approvals live in the editable .memcode/permissions file,
-- not the DB — security policy is the user's to read and edit, not opaque rows.)

-- Adjudicated claims extracted from sources + deterministic evidence. Status
-- distinguishes what currently governs reality from what is stale/conflicted.
CREATE TABLE IF NOT EXISTS claims (
    id                 TEXT PRIMARY KEY,
    claim_type         TEXT NOT NULL,   -- doctrine | preference | command | decision | warning
    text               TEXT NOT NULL,
    scope              TEXT,            -- dir it governs ("." = repo)
    status             TEXT NOT NULL,   -- candidate | current | stale | conflicted | rejected
    confidence         TEXT,            -- low | medium | high
    evidence           TEXT,
    source_path        TEXT,
    source_modified_at TEXT,
    extracted_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_claims_status ON claims (status);

-- Preference candidates: behavior-derived directives that accumulated enough
-- weighted evidence (via the prefs reducer) to be promoted to standing rules.
-- The event log (preference_signal events) is the source of truth; this table
-- is a materialized projection rebuilt by prefs.Reduce.
CREATE TABLE IF NOT EXISTS preference_candidates (
    id              TEXT PRIMARY KEY,
    axis            TEXT NOT NULL,
    text            TEXT NOT NULL,
    scope           TEXT NOT NULL DEFAULT '.',
    weight          REAL NOT NULL,
    signal_count    INTEGER NOT NULL DEFAULT 0,
    session_count   INTEGER NOT NULL DEFAULT 0,
    first_seen      TEXT NOT NULL,
    last_seen       TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'candidate',  -- candidate | confirmed | demoted
    confirmed_path  TEXT,
    evidence_json   TEXT
);
CREATE INDEX IF NOT EXISTS idx_prefcand_status ON preference_candidates (status);

-- Materialized current state, split by layer (churn rate).
CREATE TABLE IF NOT EXISTS current_state (
    scope            TEXT NOT NULL,        -- repo | subsystem:<x>
    layer            TEXT NOT NULL,        -- structural | doctrine | journey
    body_json        TEXT,
    source_event_id  INTEGER,
    refreshed_at     TEXT,
    PRIMARY KEY (scope, layer)
);
