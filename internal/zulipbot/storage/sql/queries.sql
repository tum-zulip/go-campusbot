-- name: SchemaVersion :one
SELECT CAST(COALESCE(MAX(version), 0) AS INTEGER) AS version
FROM schema_migrations;

-- name: SchemaMigrationName :one
SELECT name
FROM schema_migrations
WHERE version = ?;

-- name: GetConfigValue :one
SELECT value
FROM bot_config
WHERE key = ?;

-- name: SetConfigValue :exec
INSERT INTO bot_config(key, value, updated_by_user_id, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
  value = excluded.value,
  updated_by_user_id = excluded.updated_by_user_id,
  updated_at = excluded.updated_at;

-- name: GetEventQueueState :one
SELECT queue_id, last_event_id, updated_at
FROM event_queue_state
WHERE id = 1;

-- name: SaveEventQueueState :exec
INSERT INTO event_queue_state(id, queue_id, last_event_id, updated_at)
VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  queue_id = excluded.queue_id,
  last_event_id = excluded.last_event_id,
  updated_at = excluded.updated_at;

-- name: ClearEventQueueState :exec
DELETE FROM event_queue_state
WHERE id = 1;

-- name: GetProcessedMessage :one
SELECT message_id
FROM processed_messages
WHERE message_id = ?;

-- name: MarkMessageProcessed :exec
INSERT OR IGNORE INTO processed_messages(message_id, processed_at)
VALUES (?, ?);

-- name: DeleteExpiredProcessedMessages :execrows
DELETE FROM processed_messages
WHERE processed_at < ?;

-- name: TrimProcessedMessages :execrows
DELETE FROM processed_messages
WHERE message_id NOT IN (
  SELECT message_id
  FROM processed_messages
  ORDER BY processed_at DESC, message_id DESC
  LIMIT ?
);

-- name: CountProcessedMessages :one
SELECT COUNT(*) AS count
FROM processed_messages;

-- name: CreateRestartRequest :exec
INSERT OR IGNORE INTO restart_requests(
  status,
  requested_by_user_id,
  request_message_id,
  response_kind,
  channel_id,
  topic,
  recipient_user_ids,
  requested_at
) VALUES ('requested', ?, ?, ?, ?, ?, ?, ?);

-- name: GetRestartRequestIDByMessageID :one
SELECT id
FROM restart_requests
WHERE request_message_id = ?;

-- name: GetPendingRestartRequest :one
SELECT
  id,
  requested_by_user_id,
  request_message_id,
  response_kind,
  channel_id,
  topic,
  recipient_user_ids,
  requested_at
FROM restart_requests
WHERE status IN ('requested', 'in_progress')
ORDER BY id DESC
LIMIT 1;

-- name: MarkRestartInProgress :execrows
UPDATE restart_requests
SET status = ?, completed_at = NULL, completion_message_id = NULL, failure = NULL
WHERE id = ? AND status IN ('requested', 'in_progress');

-- name: GetLatestActiveRestartRequestID :one
SELECT id
FROM restart_requests
WHERE status IN ('requested', 'in_progress')
ORDER BY id DESC
LIMIT 1;

-- name: CompleteRestartRequest :exec
UPDATE restart_requests
SET status = ?, completed_at = ?, completion_message_id = ?, failure = ?
WHERE id = ?;

-- name: ListAuditRecords :many
SELECT
  at,
  actor_user_id,
  action,
  target,
  status,
  message_id,
  old_value,
  new_value,
  error
FROM audit_log
ORDER BY id;

-- name: RecordAudit :exec
INSERT INTO audit_log(
  at,
  actor_user_id,
  action,
  target,
  status,
  message_id,
  old_value,
  new_value,
  error
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- emoji_group_mappings

-- name: UpsertEmojiGroupMapping :exec
INSERT INTO emoji_group_mappings (short_name, channel_group_id, emoji_name, emoji_code, reaction_type, enabled, sort_order, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(short_name) DO UPDATE SET
  channel_group_id = excluded.channel_group_id,
  emoji_name = excluded.emoji_name,
  emoji_code = excluded.emoji_code,
  reaction_type = excluded.reaction_type,
  enabled = excluded.enabled,
  sort_order = excluded.sort_order,
  updated_at = excluded.updated_at;

-- name: ListEnabledEmojiGroupMappings :many
SELECT id, short_name, channel_group_id, emoji_name, emoji_code, reaction_type, enabled, sort_order, created_at, updated_at
FROM emoji_group_mappings
WHERE enabled = 1
ORDER BY sort_order, short_name;

-- name: ListAllEmojiGroupMappings :many
SELECT id, short_name, channel_group_id, emoji_name, emoji_code, reaction_type, enabled, sort_order, created_at, updated_at
FROM emoji_group_mappings
ORDER BY sort_order, short_name;

-- name: GetEmojiGroupMappingByShortName :one
SELECT id, short_name, channel_group_id, emoji_name, emoji_code, reaction_type, enabled, sort_order, created_at, updated_at
FROM emoji_group_mappings
WHERE short_name = ? AND enabled = 1;

-- name: GetEmojiGroupMappingByEmoji :one
SELECT id, short_name, channel_group_id, emoji_name, emoji_code, reaction_type, enabled, sort_order, created_at, updated_at
FROM emoji_group_mappings
WHERE emoji_name = ? AND reaction_type = ? AND enabled = 1;

-- name: SetEmojiGroupMappingEnabled :exec
UPDATE emoji_group_mappings SET enabled = ?, updated_at = ? WHERE short_name = ?;

-- announcement_state

-- name: GetAnnouncementState :one
SELECT id, message_id, content_hash, updated_at FROM announcement_state WHERE id = 1;

-- name: SaveAnnouncementState :exec
INSERT INTO announcement_state (id, message_id, content_hash, updated_at)
VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  message_id = excluded.message_id,
  content_hash = excluded.content_hash,
  updated_at = excluded.updated_at;

-- processed_reactions

-- name: IsReactionProcessed :one
SELECT id FROM processed_reactions
WHERE message_id = ? AND user_id = ? AND emoji_name = ? AND op = ?;

-- name: MarkReactionProcessed :exec
INSERT OR IGNORE INTO processed_reactions (message_id, user_id, emoji_name, op, processed_at)
VALUES (?, ?, ?, ?, ?);
