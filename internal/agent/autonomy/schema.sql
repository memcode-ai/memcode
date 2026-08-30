CREATE TABLE IF NOT EXISTS objectives (
  id TEXT PRIMARY KEY, description TEXT NOT NULL, success_criteria TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL, review_at TEXT
);
CREATE TABLE IF NOT EXISTS subgoals (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, parent_id TEXT, description TEXT NOT NULL,
  status TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0, rationale TEXT NOT NULL DEFAULT '',
  dependencies_json TEXT NOT NULL DEFAULT '[]', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
  FOREIGN KEY(objective_id) REFERENCES objectives(id)
);
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, subgoal_id TEXT, parent_run_id TEXT, session_id TEXT,
  envelope_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL, outcome_json TEXT, evidence_json TEXT,
  created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS triggers (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, kind TEXT NOT NULL, spec TEXT NOT NULL,
  missed_run_policy TEXT NOT NULL DEFAULT 'skip', status TEXT NOT NULL,
  next_due_at TEXT, last_fired_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS policies (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, version INTEGER NOT NULL, document_json TEXT NOT NULL,
  hash TEXT NOT NULL, status TEXT NOT NULL, approved_at TEXT, created_at TEXT NOT NULL,
  UNIQUE(objective_id, version), UNIQUE(objective_id, hash)
);
CREATE TABLE IF NOT EXISTS resources (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, type TEXT NOT NULL, locator TEXT NOT NULL,
  access_mode TEXT NOT NULL, constraints_json TEXT NOT NULL DEFAULT '{}', authorization_source TEXT NOT NULL,
  policy_hash TEXT NOT NULL, expires_at TEXT, status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS actions (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, subgoal_id TEXT, run_id TEXT, kind TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '', consequence_class TEXT NOT NULL, policy_hash TEXT NOT NULL,
  request_json TEXT NOT NULL DEFAULT '{}', idempotency_key TEXT, status TEXT NOT NULL,
  result_json TEXT, evidence_json TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS actions_idempotency ON actions(objective_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TABLE IF NOT EXISTS generated_items (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, path TEXT NOT NULL, hash TEXT NOT NULL,
  purpose TEXT NOT NULL, source_run_id TEXT, parent_revision TEXT,
  invocation_json TEXT NOT NULL DEFAULT '{}', evaluations_json TEXT NOT NULL DEFAULT '[]',
  last_used_at TEXT, active_revision TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS notifications (
  id TEXT PRIMARY KEY, objective_id TEXT NOT NULL, kind TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
