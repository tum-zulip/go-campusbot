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
