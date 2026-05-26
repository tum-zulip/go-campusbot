-- name: CreateChannelGroup :one
INSERT INTO channel_groups (
  name,
  user_group_id
) VALUES (
  ?,
  ?
)
RETURNING *;

-- name: GetChannelGroup :one
SELECT *
FROM channel_groups
WHERE id = ?;

-- name: GetChannelGroupByUserGroupID :one
SELECT *
FROM channel_groups
WHERE user_group_id = ?;

-- name: ListChannelGroups :many
SELECT *
FROM channel_groups
WHERE deactivated_at IS NULL
ORDER BY id;

-- name: SetChannelGroupDeactivated :one
UPDATE channel_groups
SET
  deactivated_at = CURRENT_TIMESTAMP,
  updated_at = CURRENT_TIMESTAMP
WHERE id = ?
RETURNING *;

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

-- name: ListChannelGroupChannels :many
SELECT channel_id
FROM channel_group_channels
WHERE channel_group_id = ?
ORDER BY channel_id;
