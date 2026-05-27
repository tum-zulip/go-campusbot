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

-- emoji -> channel group mappings
CREATE TABLE IF NOT EXISTS emoji_group_mappings (
  channel_group_id INTEGER PRIMARY KEY,
  emoji_name TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- announcement message state (single row, id always 1)
CREATE TABLE IF NOT EXISTS announcement_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  message_id INTEGER,
  content_hash TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

-- reaction event deduplication
CREATE TABLE IF NOT EXISTS processed_reactions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  emoji_name TEXT NOT NULL,
  op TEXT NOT NULL CHECK (op IN ('add', 'remove')),
  processed_at TEXT NOT NULL,
  UNIQUE(message_id, user_id, emoji_name, op)
);
