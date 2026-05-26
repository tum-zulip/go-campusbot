package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "embed"
	_ "modernc.org/sqlite"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

const currentSchemaVersion = 1

const schemaBaselineName = "development baseline"

const restartStatusInProgress = "in_progress"

//go:embed sql/schema.sql
var schemaSQL string

type Repository struct {
	db      *sql.DB
	queries *storagedb.Queries
	now     func() time.Time
}

type ConfigChange struct {
	Key              string
	Value            string
	ActorUserID      int64
	MessageID        int64
	OldValueRedacted string
	NewValueRedacted string
}

type EventQueueState struct {
	QueueID     string
	LastEventID int64
	UpdatedAt   time.Time
}

type RestartRequest struct {
	ID                int64
	RequestedByUserID int64
	RequestMessageID  int64
	Target            model.ReplyTarget
	RequestedAt       time.Time
}

func Open(ctx context.Context, path string) (*Repository, error) {
	if path == "" {
		return nil, errors.New("database path must not be empty")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	repo := &Repository{
		db:      db,
		queries: storagedb.New(db),
		now:     func() time.Time { return time.Now().UTC() },
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = repo.db.Close()
		return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		_ = repo.db.Close()
		return nil, fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = repo.db.Close()
		return nil, fmt.Errorf("enable SQLite WAL journal mode: %w", err)
	}
	if err := repo.Migrate(ctx); err != nil {
		_ = repo.db.Close()
		return nil, err
	}
	return repo, nil
}

func (repo *Repository) Close() error {
	if repo == nil || repo.db == nil {
		return nil
	}
	return repo.db.Close()
}

func (repo *Repository) Migrate(ctx context.Context) error {
	if _, err := repo.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply development SQLite schema: %w", err)
	}
	version, err := repo.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version != currentSchemaVersion {
		return fmt.Errorf(
			"database schema version %d does not match development schema version %d; reset the database or add a migration policy",
			version,
			currentSchemaVersion,
		)
	}
	name, err := repo.queries.SchemaMigrationName(ctx, int64(currentSchemaVersion))
	if err != nil {
		return fmt.Errorf("read schema baseline name: %w", err)
	}
	if name != schemaBaselineName {
		return fmt.Errorf(
			"database schema baseline %q is not compatible with the current development schema %q; reset the database or add a migration policy",
			name,
			schemaBaselineName,
		)
	}
	return nil
}

func (repo *Repository) SchemaVersion(ctx context.Context) (int, error) {
	version, err := repo.queries.SchemaVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return int(version), nil
}

func (repo *Repository) ConfigValue(ctx context.Context, key string) (string, bool, error) {
	value, err := repo.queries.GetConfigValue(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read config %q: %w", key, err)
	}
	return value, true, nil
}

func (repo *Repository) SetConfigValue(ctx context.Context, change ConfigChange) error {
	if change.Key == "" {
		return errors.New("config key must not be empty")
	}
	return repo.withTx(ctx, func(q *storagedb.Queries) error {
		now := repo.now()
		if err := q.SetConfigValue(ctx, storagedb.SetConfigValueParams{
			Key:             change.Key,
			Value:           change.Value,
			UpdatedByUserID: nullableInt64(change.ActorUserID),
			UpdatedAt:       formatTime(now),
		}); err != nil {
			return fmt.Errorf("write config %q: %w", change.Key, err)
		}
		return repo.recordAuditTx(ctx, q, audit.Record{
			At:          now,
			ActorUserID: change.ActorUserID,
			Action:      "config.set",
			Target:      change.Key,
			Status:      audit.StatusSuccess,
			MessageID:   change.MessageID,
			OldValue:    change.OldValueRedacted,
			NewValue:    change.NewValueRedacted,
		})
	})
}

func (repo *Repository) UserRole(ctx context.Context, userID int64) (permissions.Role, bool, error) {
	value, err := repo.queries.GetLocalUserRole(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read role for Zulip user %d: %w", userID, err)
	}
	role, err := parseLocalRole(value)
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

func (repo *Repository) SetUserRole(ctx context.Context, userID int64, role permissions.Role, grantedByUserID int64) error {
	if userID <= 0 {
		return errors.New("user ID must be positive")
	}
	if !role.Valid() {
		return fmt.Errorf("invalid role %q", role)
	}
	if role == permissions.RoleOwner {
		return errors.New("role 'owner' cannot be stored in the database; the bot owner is determined automatically from the Zulip API")
	}
	return repo.withTx(ctx, func(q *storagedb.Queries) error {
		if role == permissions.RoleNone {
			if err := q.DeleteLocalUserRole(ctx, userID); err != nil {
				return fmt.Errorf("delete local role for Zulip user %d: %w", userID, err)
			}
		} else {
			if err := q.SetLocalUserRole(ctx, storagedb.SetLocalUserRoleParams{
				UserID:          userID,
				Role:            string(role),
				GrantedByUserID: nullableInt64(grantedByUserID),
				UpdatedAt:       formatTime(repo.now()),
			}); err != nil {
				return fmt.Errorf("write role for Zulip user %d: %w", userID, err)
			}
		}
		return repo.recordAuditTx(ctx, q, audit.Record{
			At:          repo.now(),
			ActorUserID: grantedByUserID,
			Action:      "role.set",
			Target:      fmt.Sprintf("zulip_user:%d", userID),
			Status:      audit.StatusSuccess,
			OldValue:    "",
			NewValue:    string(role),
		})
	})
}

// UserRoleRecord is a row from user_roles.
type UserRoleRecord struct {
	UserID          int64
	Role            permissions.Role
	GrantedByUserID int64
	UpdatedAt       time.Time
}

// ListUserRoles returns all explicitly assigned local roles, newest-first.
func (repo *Repository) ListUserRoles(ctx context.Context) ([]UserRoleRecord, error) {
	rows, err := repo.queries.ListLocalUserRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("list user roles: %w", err)
	}
	records := make([]UserRoleRecord, 0, len(rows))
	for _, row := range rows {
		role, err := parseLocalRole(row.Role)
		if err != nil {
			return nil, err
		}
		updatedAt, err := parseTime(row.UpdatedAt)
		if err != nil {
			return nil, err
		}
		records = append(records, UserRoleRecord{
			UserID:          row.UserID,
			Role:            role,
			GrantedByUserID: nullInt64Value(row.GrantedByUserID),
			UpdatedAt:       updatedAt,
		})
	}
	return records, nil
}

// Ping checks if the database is reachable.
func (repo *Repository) Ping(ctx context.Context) error {
	return repo.db.PingContext(ctx)
}

func (repo *Repository) EventQueueState(ctx context.Context) (EventQueueState, bool, error) {
	row, err := repo.queries.GetEventQueueState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return EventQueueState{}, false, nil
	}
	if err != nil {
		return EventQueueState{}, false, fmt.Errorf("read event queue state: %w", err)
	}
	updatedAt, err := parseTime(row.UpdatedAt)
	if err != nil {
		return EventQueueState{}, false, err
	}
	return EventQueueState{
		QueueID:     row.QueueID,
		LastEventID: row.LastEventID,
		UpdatedAt:   updatedAt,
	}, true, nil
}

func (repo *Repository) SaveEventQueueState(ctx context.Context, state EventQueueState) error {
	if state.QueueID == "" {
		return errors.New("queue ID must not be empty")
	}
	if err := repo.queries.SaveEventQueueState(ctx, storagedb.SaveEventQueueStateParams{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
		UpdatedAt:   formatTime(repo.now()),
	}); err != nil {
		return fmt.Errorf("save event queue state: %w", err)
	}
	return nil
}

func (repo *Repository) ClearEventQueueState(ctx context.Context) error {
	if err := repo.queries.ClearEventQueueState(ctx); err != nil {
		return fmt.Errorf("clear event queue state: %w", err)
	}
	return nil
}

func (repo *Repository) MessageProcessed(ctx context.Context, messageID int64) (bool, error) {
	_, err := repo.queries.GetProcessedMessage(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read processed message %d: %w", messageID, err)
	}
	return true, nil
}

func (repo *Repository) MarkMessageProcessed(ctx context.Context, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	if err := repo.queries.MarkMessageProcessed(ctx, storagedb.MarkMessageProcessedParams{
		MessageID:   messageID,
		ProcessedAt: formatTime(repo.now()),
	}); err != nil {
		return fmt.Errorf("mark processed message %d: %w", messageID, err)
	}
	return nil
}

func (repo *Repository) CleanupProcessedMessages(ctx context.Context, retention time.Duration, maxRows int) (int64, error) {
	var deleted int64
	err := repo.withTx(ctx, func(q *storagedb.Queries) error {
		if retention > 0 {
			count, err := q.DeleteExpiredProcessedMessages(ctx, formatTime(repo.now().Add(-retention)))
			if err != nil {
				return fmt.Errorf("delete expired processed messages: %w", err)
			}
			deleted += count
		}
		if maxRows > 0 {
			count, err := q.TrimProcessedMessages(ctx, int64(maxRows))
			if err != nil {
				return fmt.Errorf("trim processed message cache: %w", err)
			}
			deleted += count
		}
		return nil
	})
	return deleted, err
}

func (repo *Repository) ProcessedMessageCount(ctx context.Context) (int, error) {
	count, err := repo.queries.CountProcessedMessages(ctx)
	if err != nil {
		return 0, fmt.Errorf("count processed messages: %w", err)
	}
	return int(count), nil
}

func (repo *Repository) CreateRestartRequest(ctx context.Context, request RestartRequest) (int64, error) {
	if request.RequestedByUserID <= 0 {
		return 0, errors.New("restart requester user ID must be positive")
	}
	if request.RequestMessageID <= 0 {
		return 0, errors.New("restart request message ID must be positive")
	}
	if err := request.Target.Validate(); err != nil {
		return 0, err
	}
	targetUsers, err := json.Marshal(request.Target.UserIDs)
	if err != nil {
		return 0, fmt.Errorf("encode restart target user IDs: %w", err)
	}

	var id int64
	err = repo.withTx(ctx, func(q *storagedb.Queries) error {
		if err := q.CreateRestartRequest(ctx, storagedb.CreateRestartRequestParams{
			RequestedByUserID: request.RequestedByUserID,
			RequestMessageID:  request.RequestMessageID,
			ResponseKind:      string(request.Target.Kind),
			ChannelID:         nullableInt64(request.Target.ChannelID),
			Topic:             nullableString(request.Target.Topic),
			RecipientUserIds:  string(targetUsers),
			RequestedAt:       formatTime(repo.now()),
		}); err != nil {
			return fmt.Errorf("create restart request: %w", err)
		}
		var err error
		id, err = q.GetRestartRequestIDByMessageID(ctx, request.RequestMessageID)
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (repo *Repository) PendingRestartRequest(ctx context.Context) (RestartRequest, bool, error) {
	row, err := repo.queries.GetPendingRestartRequest(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return RestartRequest{}, false, nil
	}
	if err != nil {
		return RestartRequest{}, false, fmt.Errorf("read pending restart request: %w", err)
	}
	return restartRequestFromRow(row)
}

func (repo *Repository) MarkRestartInProgress(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("restart request ID must be positive")
	}
	affected, err := repo.queries.MarkRestartInProgress(ctx, storagedb.MarkRestartInProgressParams{
		Status: restartStatusInProgress,
		ID:     id,
	})
	if err != nil {
		return fmt.Errorf("mark restart request %d in progress: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("restart request %d is not pending", id)
	}
	return nil
}

func (repo *Repository) LatestActiveRestartRequestID(ctx context.Context) (int64, bool, error) {
	id, err := repo.queries.GetLatestActiveRestartRequestID(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read active restart request ID: %w", err)
	}
	return id, true, nil
}

func (repo *Repository) CompleteRestartRequest(ctx context.Context, id int64, completionMessageID int64, failure string) error {
	if id <= 0 {
		return errors.New("restart request ID must be positive")
	}
	status := "completed"
	if failure != "" {
		status = "failed"
	}
	if err := repo.queries.CompleteRestartRequest(ctx, storagedb.CompleteRestartRequestParams{
		Status:              status,
		CompletedAt:         nullableString(formatTime(repo.now())),
		CompletionMessageID: nullableInt64(completionMessageID),
		Failure:             nullableString(failure),
		ID:                  id,
	}); err != nil {
		return fmt.Errorf("complete restart request %d: %w", id, err)
	}
	return nil
}

func (repo *Repository) AuditRecords(ctx context.Context) ([]audit.Record, error) {
	rows, err := repo.queries.ListAuditRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("read audit records: %w", err)
	}
	records := make([]audit.Record, 0, len(rows))
	for _, row := range rows {
		at, err := parseTime(row.At)
		if err != nil {
			return nil, err
		}
		records = append(records, audit.Record{
			At:          at,
			ActorUserID: nullInt64Value(row.ActorUserID),
			Action:      row.Action,
			Target:      nullStringValue(row.Target),
			Status:      audit.Status(row.Status),
			MessageID:   nullInt64Value(row.MessageID),
			OldValue:    nullStringValue(row.OldValue),
			NewValue:    nullStringValue(row.NewValue),
			Error:       nullStringValue(row.Error),
		})
	}
	return records, nil
}

func (repo *Repository) RecordAudit(ctx context.Context, record audit.Record) error {
	return repo.withTx(ctx, func(q *storagedb.Queries) error {
		return repo.recordAuditTx(ctx, q, record)
	})
}

func (repo *Repository) recordAuditTx(ctx context.Context, q *storagedb.Queries, record audit.Record) error {
	if record.Action == "" {
		return errors.New("audit action must not be empty")
	}
	record = record.WithTime(repo.now())
	if err := q.RecordAudit(ctx, storagedb.RecordAuditParams{
		At:          formatTime(record.At),
		ActorUserID: nullableInt64(record.ActorUserID),
		Action:      record.Action,
		Target:      nullableString(record.Target),
		Status:      string(record.Status),
		MessageID:   nullableInt64(record.MessageID),
		OldValue:    nullableString(record.OldValue),
		NewValue:    nullableString(record.NewValue),
		Error:       nullableString(record.Error),
	}); err != nil {
		return fmt.Errorf("record audit event %q: %w", record.Action, err)
	}
	return nil
}

// RawEvent is a row from raw_events.
type RawEvent struct {
	QueueID    string
	EventID    int64
	EventType  string
	ReceivedAt time.Time
	RawJSON    []byte
}

// LifecycleKind identifies the type of channel lifecycle event in the persistent queue.
type LifecycleKind string

const (
	LifecycleKindChannelCreated         LifecycleKind = "channel_created"
	LifecycleKindChannelUpdated         LifecycleKind = "channel_updated"
	LifecycleKindChannelDeleted         LifecycleKind = "channel_deleted"
	LifecycleKindSubscriptionAdded      LifecycleKind = "subscription_added"
	LifecycleKindSubscriptionRemoved    LifecycleKind = "subscription_removed"
	LifecycleKindSubscriptionPeerAdd    LifecycleKind = "subscription_peer_add"
	LifecycleKindSubscriptionPeerRemove LifecycleKind = "subscription_peer_remove"
)

// LifecycleStatus is the processing status of a channel lifecycle queue entry.
type LifecycleStatus string

const (
	LifecycleStatusPending    LifecycleStatus = "pending"
	LifecycleStatusProcessing LifecycleStatus = "processing"
	LifecycleStatusDone       LifecycleStatus = "done"
	LifecycleStatusFailed     LifecycleStatus = "failed"
	LifecycleStatusSkipped    LifecycleStatus = "skipped"
)

// ChannelLifecycleEnqueueItem is an item to insert into the channel lifecycle queue.
type ChannelLifecycleEnqueueItem struct {
	LifecycleKind LifecycleKind
	ChannelID     *int64
	ChannelName   *string
	Op            string
}

// ChannelLifecycleEntry is a row from channel_lifecycle_queue.
type ChannelLifecycleEntry struct {
	ID             int64
	ZulipEventID   int64
	ZulipEventType string
	LifecycleKind  LifecycleKind
	ChannelID      *int64
	ChannelName    *string
	Op             string
	PayloadJSON    []byte
	Status         LifecycleStatus
	Attempts       int
	AvailableAt    time.Time
	LockedAt       *time.Time
	LockedBy       string
	ProcessedAt    *time.Time
	LastError      string
	CreatedAt      time.Time
}

// LifecycleQueueStatusCount is a status count for the lifecycle queue.
type LifecycleQueueStatusCount struct {
	Status LifecycleStatus
	Count  int
}

// ChannelLifecycleQueue is the interface for the persistent channel lifecycle queue.
// It is implemented by *Repository. Defined here so future workers can depend on
// an interface rather than the concrete Repository type.
type ChannelLifecycleQueue interface {
	ListPendingChannelLifecycleEntries(ctx context.Context, now time.Time, limit int) ([]ChannelLifecycleEntry, error)
	ClaimChannelLifecycleEntry(ctx context.Context, id int64, lockedBy string) (bool, error)
	MarkChannelLifecycleEntryDone(ctx context.Context, id int64) error
	MarkChannelLifecycleEntryFailed(ctx context.Context, id int64, errMsg string) error
	MarkChannelLifecycleEntrySkipped(ctx context.Context, id int64) error
	ResetChannelLifecycleEntryToPending(ctx context.Context, id int64) (bool, error)
	ResetAllFailedChannelLifecycleEntries(ctx context.Context) (int64, error)
	GetChannelLifecycleEntry(ctx context.Context, id int64) (ChannelLifecycleEntry, bool, error)
	ChannelLifecycleQueueStatusCounts(ctx context.Context) ([]LifecycleQueueStatusCount, error)
	CleanupChannelLifecycleEntries(ctx context.Context, retention time.Duration) (int64, error)
}

// Compile-time check that *Repository satisfies ChannelLifecycleQueue.
var _ ChannelLifecycleQueue = (*Repository)(nil)

// StoreRawEvent inserts a raw event into the raw_events table.
// It uses INSERT OR IGNORE so duplicate events are silently skipped.
func (repo *Repository) StoreRawEvent(ctx context.Context, event RawEvent) error {
	if err := repo.queries.StoreRawEvent(ctx, storagedb.StoreRawEventParams{
		QueueID:    event.QueueID,
		EventID:    event.EventID,
		EventType:  event.EventType,
		ReceivedAt: formatTime(event.ReceivedAt),
		RawJson:    string(event.RawJSON),
	}); err != nil {
		return fmt.Errorf("store raw event %d: %w", event.EventID, err)
	}
	return nil
}

// CleanupRawEvents removes old raw events beyond the retention window or row cap.
func (repo *Repository) CleanupRawEvents(ctx context.Context, retention time.Duration, maxRows int) (int64, error) {
	var deleted int64
	err := repo.withTx(ctx, func(q *storagedb.Queries) error {
		if retention > 0 {
			count, err := q.DeleteExpiredRawEvents(ctx, formatTime(repo.now().Add(-retention)))
			if err != nil {
				return fmt.Errorf("delete expired raw events: %w", err)
			}
			deleted += count
		}
		if maxRows > 0 {
			count, err := q.TrimRawEvents(ctx, int64(maxRows))
			if err != nil {
				return fmt.Errorf("trim raw events cache: %w", err)
			}
			deleted += count
		}
		return nil
	})
	return deleted, err
}

// RawEventCount returns the number of rows in raw_events.
func (repo *Repository) RawEventCount(ctx context.Context) (int, error) {
	count, err := repo.queries.CountRawEvents(ctx)
	if err != nil {
		return 0, fmt.Errorf("count raw events: %w", err)
	}
	return int(count), nil
}

// StoreRawEventAndEnqueueLifecycle stores a raw event and derived channel lifecycle items
// in one transaction. Both inserts use INSERT OR IGNORE so duplicate deliveries are safely
// skipped. If items is empty, only the raw event is stored.
// Failure is returned; callers should treat this as non-fatal (log and continue).
func (repo *Repository) StoreRawEventAndEnqueueLifecycle(ctx context.Context, event RawEvent, items []ChannelLifecycleEnqueueItem) error {
	return repo.withTx(ctx, func(q *storagedb.Queries) error {
		if err := q.StoreRawEvent(ctx, storagedb.StoreRawEventParams{
			QueueID:    event.QueueID,
			EventID:    event.EventID,
			EventType:  event.EventType,
			ReceivedAt: formatTime(event.ReceivedAt),
			RawJson:    string(event.RawJSON),
		}); err != nil {
			return fmt.Errorf("store raw event %d: %w", event.EventID, err)
		}
		now := repo.now()
		for _, item := range items {
			if err := q.EnqueueChannelLifecycleItem(ctx, storagedb.EnqueueChannelLifecycleItemParams{
				ZulipEventID:   event.EventID,
				ZulipEventType: event.EventType,
				LifecycleKind:  string(item.LifecycleKind),
				ChannelID:      nullableInt64Ptr(item.ChannelID),
				ChannelName:    nullableStringPtr(item.ChannelName),
				Op:             nullableString(item.Op),
				PayloadJson:    string(event.RawJSON),
				AvailableAt:    formatTime(now),
				CreatedAt:      formatTime(now),
			}); err != nil {
				return fmt.Errorf("enqueue lifecycle item %s for event %d: %w", item.LifecycleKind, event.EventID, err)
			}
		}
		return nil
	})
}

// ListPendingChannelLifecycleEntries returns up to limit pending entries available at or
// before now, ordered by available_at ASC, id ASC (deterministic FIFO order).
func (repo *Repository) ListPendingChannelLifecycleEntries(ctx context.Context, now time.Time, limit int) ([]ChannelLifecycleEntry, error) {
	rows, err := repo.queries.ListPendingChannelLifecycleEntries(ctx, storagedb.ListPendingChannelLifecycleEntriesParams{
		AvailableAt: formatTime(now),
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list pending channel lifecycle entries: %w", err)
	}
	return channelLifecycleEntriesFromRows(rows)
}

// ClaimChannelLifecycleEntry atomically claims a pending entry by ID.
// Returns true if the entry was claimed (status changed from pending to processing).
// Returns false if the entry was not pending (already claimed or processed).
func (repo *Repository) ClaimChannelLifecycleEntry(ctx context.Context, id int64, lockedBy string) (bool, error) {
	affected, err := repo.queries.ClaimChannelLifecycleEntry(ctx, storagedb.ClaimChannelLifecycleEntryParams{
		LockedAt: nullableString(formatTime(repo.now())),
		LockedBy: nullableString(lockedBy),
		ID:       id,
	})
	if err != nil {
		return false, fmt.Errorf("claim channel lifecycle entry %d: %w", id, err)
	}
	return affected > 0, nil
}

// MarkChannelLifecycleEntryDone marks an entry as done.
func (repo *Repository) MarkChannelLifecycleEntryDone(ctx context.Context, id int64) error {
	if err := repo.queries.MarkChannelLifecycleEntryDone(ctx, storagedb.MarkChannelLifecycleEntryDoneParams{
		ProcessedAt: nullableString(formatTime(repo.now())),
		ID:          id,
	}); err != nil {
		return fmt.Errorf("mark channel lifecycle entry %d done: %w", id, err)
	}
	return nil
}

// MarkChannelLifecycleEntryFailed marks an entry as failed with an error message.
func (repo *Repository) MarkChannelLifecycleEntryFailed(ctx context.Context, id int64, errMsg string) error {
	if err := repo.queries.MarkChannelLifecycleEntryFailed(ctx, storagedb.MarkChannelLifecycleEntryFailedParams{
		LastError:   nullableString(errMsg),
		ProcessedAt: nullableString(formatTime(repo.now())),
		ID:          id,
	}); err != nil {
		return fmt.Errorf("mark channel lifecycle entry %d failed: %w", id, err)
	}
	return nil
}

// MarkChannelLifecycleEntrySkipped marks an entry as skipped.
func (repo *Repository) MarkChannelLifecycleEntrySkipped(ctx context.Context, id int64) error {
	if err := repo.queries.MarkChannelLifecycleEntrySkipped(ctx, storagedb.MarkChannelLifecycleEntrySkippedParams{
		ProcessedAt: nullableString(formatTime(repo.now())),
		ID:          id,
	}); err != nil {
		return fmt.Errorf("mark channel lifecycle entry %d skipped: %w", id, err)
	}
	return nil
}

// ResetChannelLifecycleEntryToPending resets a single entry back to pending for replay.
func (repo *Repository) ResetChannelLifecycleEntryToPending(ctx context.Context, id int64) (bool, error) {
	affected, err := repo.queries.ResetChannelLifecycleEntryToPending(ctx, storagedb.ResetChannelLifecycleEntryToPendingParams{
		AvailableAt: formatTime(repo.now()),
		ID:          id,
	})
	if err != nil {
		return false, fmt.Errorf("reset channel lifecycle entry %d to pending: %w", id, err)
	}
	return affected > 0, nil
}

// ResetAllFailedChannelLifecycleEntries resets all failed entries to pending for retry/replay.
func (repo *Repository) ResetAllFailedChannelLifecycleEntries(ctx context.Context) (int64, error) {
	count, err := repo.queries.ResetAllFailedChannelLifecycleEntries(ctx, formatTime(repo.now()))
	if err != nil {
		return 0, fmt.Errorf("reset all failed channel lifecycle entries: %w", err)
	}
	return count, nil
}

// GetChannelLifecycleEntry reads a single entry by ID.
func (repo *Repository) GetChannelLifecycleEntry(ctx context.Context, id int64) (ChannelLifecycleEntry, bool, error) {
	row, err := repo.queries.GetChannelLifecycleEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ChannelLifecycleEntry{}, false, nil
	}
	if err != nil {
		return ChannelLifecycleEntry{}, false, fmt.Errorf("get channel lifecycle entry %d: %w", id, err)
	}
	entry, err := channelLifecycleEntryFromRow(row)
	if err != nil {
		return ChannelLifecycleEntry{}, false, err
	}
	return entry, true, nil
}

// ChannelLifecycleQueueStatusCounts returns the count of entries by status.
func (repo *Repository) ChannelLifecycleQueueStatusCounts(ctx context.Context) ([]LifecycleQueueStatusCount, error) {
	rows, err := repo.queries.CountChannelLifecycleEntriesByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("count channel lifecycle entries by status: %w", err)
	}
	counts := make([]LifecycleQueueStatusCount, 0, len(rows))
	for _, row := range rows {
		counts = append(counts, LifecycleQueueStatusCount{
			Status: LifecycleStatus(row.Status),
			Count:  int(row.Count),
		})
	}
	return counts, nil
}

// CleanupChannelLifecycleEntries deletes done/skipped entries older than the retention period.
func (repo *Repository) CleanupChannelLifecycleEntries(ctx context.Context, retention time.Duration) (int64, error) {
	count, err := repo.queries.DeleteCompletedChannelLifecycleEntries(ctx,
		nullableString(formatTime(repo.now().Add(-retention))))
	if err != nil {
		return 0, fmt.Errorf("cleanup channel lifecycle entries: %w", err)
	}
	return count, nil
}

// ChannelLifecycleQueueEntryCount returns the total number of entries in the lifecycle queue.
func (repo *Repository) ChannelLifecycleQueueEntryCount(ctx context.Context) (int, error) {
	count, err := repo.queries.CountAllChannelLifecycleEntries(ctx)
	if err != nil {
		return 0, fmt.Errorf("count all channel lifecycle entries: %w", err)
	}
	return int(count), nil
}

func (repo *Repository) withTx(ctx context.Context, fn func(*storagedb.Queries) error) error {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := fn(repo.queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite transaction: %w", err)
	}
	tx = nil
	return nil
}

func restartRequestFromRow(row storagedb.GetPendingRestartRequestRow) (RestartRequest, bool, error) {
	var userIDs []int64
	if err := json.Unmarshal([]byte(row.RecipientUserIds), &userIDs); err != nil {
		return RestartRequest{}, false, fmt.Errorf("decode restart target user IDs: %w", err)
	}
	requestedAt, err := parseTime(row.RequestedAt)
	if err != nil {
		return RestartRequest{}, false, err
	}
	request := RestartRequest{
		ID:                row.ID,
		RequestedByUserID: row.RequestedByUserID,
		RequestMessageID:  row.RequestMessageID,
		RequestedAt:       requestedAt,
		Target: model.ReplyTarget{
			Kind:    model.ReplyKind(row.ResponseKind),
			Topic:   nullStringValue(row.Topic),
			UserIDs: userIDs,
		},
	}
	if row.ChannelID.Valid {
		request.Target.ChannelID = row.ChannelID.Int64
	}
	return request, true, nil
}

func parseLocalRole(value string) (permissions.Role, error) {
	role, err := permissions.ParseRole(value)
	if err != nil {
		return "", err
	}
	if role == permissions.RoleOwner {
		return "", errors.New("invalid local role 'owner'; bot owner is Zulip-derived")
	}
	return role, nil
}

func nullableInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: value != 0}
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func nullInt64Value(value sql.NullInt64) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func nullStringValue(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored timestamp %q: %w", value, err)
	}
	return parsed, nil
}

func nullableInt64Ptr(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func nullableStringPtr(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: *v != ""}
}

func channelLifecycleEntryFromRow(row storagedb.ChannelLifecycleQueue) (ChannelLifecycleEntry, error) {
	availableAt, err := parseTime(row.AvailableAt)
	if err != nil {
		return ChannelLifecycleEntry{}, err
	}
	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return ChannelLifecycleEntry{}, err
	}
	entry := ChannelLifecycleEntry{
		ID:             row.ID,
		ZulipEventID:   row.ZulipEventID,
		ZulipEventType: row.ZulipEventType,
		LifecycleKind:  LifecycleKind(row.LifecycleKind),
		Op:             nullStringValue(row.Op),
		PayloadJSON:    []byte(row.PayloadJson),
		Status:         LifecycleStatus(row.Status),
		Attempts:       int(row.Attempts),
		AvailableAt:    availableAt,
		LockedBy:       nullStringValue(row.LockedBy),
		LastError:      nullStringValue(row.LastError),
		CreatedAt:      createdAt,
	}
	if row.ChannelID.Valid {
		entry.ChannelID = &row.ChannelID.Int64
	}
	if row.ChannelName.Valid && row.ChannelName.String != "" {
		entry.ChannelName = &row.ChannelName.String
	}
	if row.LockedAt.Valid {
		t, err := parseTime(row.LockedAt.String)
		if err != nil {
			return ChannelLifecycleEntry{}, err
		}
		entry.LockedAt = &t
	}
	if row.ProcessedAt.Valid {
		t, err := parseTime(row.ProcessedAt.String)
		if err != nil {
			return ChannelLifecycleEntry{}, err
		}
		entry.ProcessedAt = &t
	}
	return entry, nil
}

func channelLifecycleEntriesFromRows(rows []storagedb.ChannelLifecycleQueue) ([]ChannelLifecycleEntry, error) {
	entries := make([]ChannelLifecycleEntry, 0, len(rows))
	for _, row := range rows {
		entry, err := channelLifecycleEntryFromRow(row)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
