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

-- name: GetLocalUserRole :one
SELECT role
FROM user_roles
WHERE user_id = ?;

-- name: SetLocalUserRole :exec
INSERT INTO user_roles(user_id, role, granted_by_user_id, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(user_id) DO UPDATE SET
  role = excluded.role,
  granted_by_user_id = excluded.granted_by_user_id,
  updated_at = excluded.updated_at;

-- name: DeleteLocalUserRole :exec
DELETE FROM user_roles
WHERE user_id = ?;

-- name: ListLocalUserRoles :many
SELECT user_id, role, granted_by_user_id, updated_at
FROM user_roles
ORDER BY updated_at DESC, user_id ASC;

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
