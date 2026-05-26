CREATE TABLE IF NOT EXISTS schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

INSERT OR IGNORE INTO schema_migrations(version, name, applied_at)
VALUES (1, 'development baseline', '1970-01-01T00:00:00Z');

CREATE TABLE IF NOT EXISTS bot_config (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_by_user_id INTEGER,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS event_queue_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  queue_id TEXT NOT NULL,
  last_event_id INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS processed_messages (
  message_id INTEGER PRIMARY KEY,
  processed_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_processed_messages_processed_at
  ON processed_messages(processed_at, message_id);

CREATE TABLE IF NOT EXISTS restart_requests (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  status TEXT NOT NULL CHECK (status IN ('requested', 'in_progress', 'completed', 'failed')),
  requested_by_user_id INTEGER NOT NULL,
  request_message_id INTEGER NOT NULL UNIQUE,
  response_kind TEXT NOT NULL CHECK (response_kind IN ('channel', 'direct')),
  channel_id INTEGER,
  topic TEXT,
  recipient_user_ids TEXT NOT NULL,
  requested_at TEXT NOT NULL,
  completed_at TEXT,
  completion_message_id INTEGER,
  failure TEXT
);

CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL,
  actor_user_id INTEGER,
  action TEXT NOT NULL,
  target TEXT,
  status TEXT NOT NULL,
  message_id INTEGER,
  old_value TEXT,
  new_value TEXT,
  error TEXT
);
