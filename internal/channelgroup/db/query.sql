-- name: CreateChannelGroup :one
INSERT INTO channel_groups (
  id
) VALUES (
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

-- name: ListChannelGroupChannels :many
SELECT channel_id
FROM channel_group_channels
WHERE channel_group_id = ?
ORDER BY channel_id;
