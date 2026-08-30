CREATE TABLE IF NOT EXISTS interactions (
  id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, objective_id TEXT NOT NULL, run_id TEXT NOT NULL,
  kind TEXT NOT NULL, question TEXT NOT NULL, context TEXT NOT NULL DEFAULT '', answer TEXT,
  status TEXT NOT NULL DEFAULT 'pending', tool_use_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL, answered_at TEXT
);
CREATE INDEX IF NOT EXISTS idx_interactions_agent ON interactions(agent_id, status);