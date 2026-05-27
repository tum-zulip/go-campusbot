-- name: CreateChannelGroup :one
INSERT INTO channel_groups (
  id,
  channel_folder_id
) VALUES (
  ?,
  ?
)
RETURNING *;

-- name: GetChannelGroup :one
SELECT *
FROM channel_groups
WHERE id = ?;

-- name: DeleteChannelGroup :exec
DELETE FROM channel_groups
WHERE id = ?;

-- name: UpdateChannelGroupChannelFolder :exec
UPDATE channel_groups
SET channel_folder_id = ?
WHERE id = ?;

-- name: ListChannelGroups :many
SELECT *
FROM channel_groups
ORDER BY id;

-- name: AddChannelGroupChannel :exec
INSERT OR IGNORE INTO channel_group_channels (
  channel_group_id,
  channel_id
) VALUES (
  ?,
  ?
);

-- name: RemoveChannelGroupChannel :exec
DELETE FROM channel_group_channels
WHERE channel_group_id = ?
  AND channel_id = ?;

-- name: RemoveChannelFromChannelGroups :exec
DELETE FROM channel_group_channels
WHERE channel_id = ?;

-- name: ListChannelGroupChannels :many
SELECT channel_id
FROM channel_group_channels
WHERE channel_group_id = ?
ORDER BY channel_id;

-- name: ListOtherChannelGroupsForChannelsInGroup :many
SELECT channel_id, channel_group_id
FROM channel_group_channels AS other_group_channels
WHERE other_group_channels.channel_group_id != sqlc.arg(channel_group_id)
  AND other_group_channels.channel_id IN (
    SELECT current_group_channels.channel_id
    FROM channel_group_channels AS current_group_channels
    WHERE current_group_channels.channel_group_id = sqlc.arg(channel_group_id)
  )
ORDER BY channel_id, channel_group_id;

-- name: GetChannelGroupEventQueueState :one
SELECT queue_id, last_event_id, updated_at
FROM channel_group_event_queue_state
WHERE id = 1;

-- name: SaveChannelGroupEventQueueState :exec
INSERT INTO channel_group_event_queue_state(id, queue_id, last_event_id, updated_at)
VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  queue_id = excluded.queue_id,
  last_event_id = excluded.last_event_id,
  updated_at = excluded.updated_at;

-- name: ClearChannelGroupEventQueueState :exec
DELETE FROM channel_group_event_queue_state
WHERE id = 1;
