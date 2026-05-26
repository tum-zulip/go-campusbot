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

CREATE TABLE IF NOT EXISTS user_roles (
  user_id INTEGER PRIMARY KEY,
  role TEXT NOT NULL CHECK (role IN ('admin', 'none')),
  granted_by_user_id INTEGER,
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

CREATE TABLE IF NOT EXISTS raw_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  queue_id TEXT NOT NULL,
  event_id INTEGER NOT NULL,
  event_type TEXT NOT NULL,
  received_at TEXT NOT NULL,
  raw_json TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_raw_events_received_at
  ON raw_events(received_at, id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_raw_events_queue_event
  ON raw_events(queue_id, event_id);

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

CREATE TABLE IF NOT EXISTS channel_lifecycle_queue (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  zulip_event_id INTEGER NOT NULL,
  zulip_event_type TEXT NOT NULL,
  lifecycle_kind TEXT NOT NULL CHECK (lifecycle_kind IN (
    'channel_created', 'channel_updated', 'channel_deleted',
    'subscription_added', 'subscription_removed',
    'subscription_peer_add', 'subscription_peer_remove'
  )),
  channel_id INTEGER,
  channel_name TEXT,
  op TEXT,
  payload_json TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'done', 'failed', 'skipped')),
  attempts INTEGER NOT NULL DEFAULT 0,
  available_at TEXT NOT NULL,
  locked_at TEXT,
  locked_by TEXT,
  processed_at TEXT,
  last_error TEXT,
  created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_lifecycle_queue_event_kind
  ON channel_lifecycle_queue(zulip_event_id, lifecycle_kind);

CREATE INDEX IF NOT EXISTS idx_channel_lifecycle_queue_pending
  ON channel_lifecycle_queue(available_at, id)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_channel_lifecycle_queue_status
  ON channel_lifecycle_queue(status, id);
