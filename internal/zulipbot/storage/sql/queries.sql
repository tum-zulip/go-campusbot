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

-- name: StoreRawEvent :exec
INSERT OR IGNORE INTO raw_events(queue_id, event_id, event_type, received_at, raw_json)
VALUES (?, ?, ?, ?, ?);

-- name: DeleteExpiredRawEvents :execrows
DELETE FROM raw_events
WHERE received_at < ?;

-- name: TrimRawEvents :execrows
DELETE FROM raw_events
WHERE id NOT IN (
  SELECT id
  FROM raw_events
  ORDER BY received_at DESC, id DESC
  LIMIT ?
);

-- name: CountRawEvents :one
SELECT COUNT(*) AS count
FROM raw_events;

-- name: EnqueueChannelLifecycleItem :exec
INSERT OR IGNORE INTO channel_lifecycle_queue(
  zulip_event_id, zulip_event_type, lifecycle_kind,
  channel_id, channel_name, op, payload_json,
  status, attempts, available_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?);

-- name: ListPendingChannelLifecycleEntries :many
SELECT id, zulip_event_id, zulip_event_type, lifecycle_kind,
  channel_id, channel_name, op, payload_json,
  status, attempts, available_at, locked_at, locked_by, processed_at, last_error, created_at
FROM channel_lifecycle_queue
WHERE status = 'pending' AND available_at <= ?
ORDER BY available_at ASC, id ASC
LIMIT ?;

-- name: ClaimChannelLifecycleEntry :execrows
UPDATE channel_lifecycle_queue
SET status = 'processing', locked_at = ?, locked_by = ?, attempts = attempts + 1
WHERE id = ? AND status = 'pending';

-- name: MarkChannelLifecycleEntryDone :exec
UPDATE channel_lifecycle_queue
SET status = 'done', processed_at = ?, locked_at = NULL, locked_by = NULL
WHERE id = ?;

-- name: MarkChannelLifecycleEntryFailed :exec
UPDATE channel_lifecycle_queue
SET status = 'failed', last_error = ?, processed_at = ?, locked_at = NULL, locked_by = NULL
WHERE id = ?;

-- name: MarkChannelLifecycleEntrySkipped :exec
UPDATE channel_lifecycle_queue
SET status = 'skipped', processed_at = ?, locked_at = NULL, locked_by = NULL
WHERE id = ?;

-- name: ResetChannelLifecycleEntryToPending :execrows
UPDATE channel_lifecycle_queue
SET status = 'pending', locked_at = NULL, locked_by = NULL, processed_at = NULL, last_error = NULL, available_at = ?
WHERE id = ?;

-- name: ResetAllFailedChannelLifecycleEntries :execrows
UPDATE channel_lifecycle_queue
SET status = 'pending', locked_at = NULL, locked_by = NULL, processed_at = NULL, last_error = NULL, available_at = ?
WHERE status = 'failed';

-- name: GetChannelLifecycleEntry :one
SELECT id, zulip_event_id, zulip_event_type, lifecycle_kind,
  channel_id, channel_name, op, payload_json,
  status, attempts, available_at, locked_at, locked_by, processed_at, last_error, created_at
FROM channel_lifecycle_queue
WHERE id = ?;

-- name: CountChannelLifecycleEntriesByStatus :many
SELECT status, CAST(COUNT(*) AS INTEGER) AS count
FROM channel_lifecycle_queue
GROUP BY status
ORDER BY status ASC;

-- name: DeleteCompletedChannelLifecycleEntries :execrows
DELETE FROM channel_lifecycle_queue
WHERE status IN ('done', 'skipped') AND processed_at < ?;

-- name: CountAllChannelLifecycleEntries :one
SELECT CAST(COUNT(*) AS INTEGER) AS count FROM channel_lifecycle_queue;
