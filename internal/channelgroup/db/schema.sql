CREATE TABLE channel_groups (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  user_group_id INTEGER NOT NULL UNIQUE,
  deactivated_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE channel_group_channels (
  channel_group_id INTEGER NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL,
  PRIMARY KEY (channel_group_id, channel_id)
);

CREATE INDEX channel_group_channels_channel_id_idx
  ON channel_group_channels(channel_id);
