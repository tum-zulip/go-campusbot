CREATE TABLE IF NOT EXISTS channelgroup_schema_migrations (
  version INTEGER PRIMARY KEY,
  name TEXT NOT NULL,
  applied_at TEXT NOT NULL
);

INSERT OR IGNORE INTO channelgroup_schema_migrations(version, name, applied_at)
VALUES (1, 'channelgroup baseline', '1970-01-01T00:00:00Z');

CREATE TABLE IF NOT EXISTS channel_groups (
  id INTEGER PRIMARY KEY,
  channel_folder_id INTEGER
);

CREATE TABLE IF NOT EXISTS channel_group_channels (
  channel_group_id INTEGER NOT NULL REFERENCES channel_groups(id) ON DELETE CASCADE,
  channel_id INTEGER NOT NULL,
  PRIMARY KEY (channel_group_id, channel_id)
);

CREATE INDEX IF NOT EXISTS channel_group_channels_channel_id_idx
  ON channel_group_channels(channel_id);
