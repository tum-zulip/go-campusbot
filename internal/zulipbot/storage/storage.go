package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "embed"

	// Import the SQLite driver.
	_ "github.com/mattn/go-sqlite3"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

const currentSchemaVersion = 1

// CurrentSchemaVersion exposes the expected schema version for tests.
const CurrentSchemaVersion = currentSchemaVersion

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
	Target            command.ReplyTarget
	RequestedAt       time.Time
}

func Open(ctx context.Context, path string) (*Repository, error) {
	if path == "" {
		return nil, errors.New("database path must not be empty")
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	repo, err := New(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func New(ctx context.Context, db *sql.DB) (*Repository, error) {
	if db == nil {
		return nil, errors.New("database connection must not be nil")
	}
	repo := &Repository{
		db:      db,
		queries: storagedb.New(db),
		now:     func() time.Time { return time.Now().UTC() },
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return nil, fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return nil, fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	if _, err := repo.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return nil, fmt.Errorf("enable SQLite WAL journal mode: %w", err)
	}
	if err := repo.Migrate(ctx); err != nil {
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

// SetNowForTest overrides the clock used by the repository.
func (repo *Repository) SetNowForTest(now func() time.Time) {
	if repo == nil {
		return
	}
	if now == nil {
		repo.now = func() time.Time { return time.Now().UTC() }
		return
	}
	repo.now = now
}

// DB returns the underlying sql.DB for use by packages that require direct access.
func (repo *Repository) DB() *sql.DB {
	return repo.db
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
	return repo.WithTx(ctx, func(q *storagedb.Queries) error {
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

func (repo *Repository) CleanupProcessedMessages(
	ctx context.Context,
	retention time.Duration,
	maxRows int,
) (int64, error) {
	var deleted int64
	err := repo.WithTx(ctx, func(q *storagedb.Queries) error {
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
	err = repo.WithTx(ctx, func(q *storagedb.Queries) error {
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

func (repo *Repository) CompleteRestartRequest(
	ctx context.Context,
	id int64,
	completionMessageID int64,
	failure string,
) error {
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
	return repo.WithTx(ctx, func(q *storagedb.Queries) error {
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

// WithTx runs fn inside a transaction.
func (repo *Repository) WithTx(ctx context.Context, fn func(*storagedb.Queries) error) error {
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
		Target: command.ReplyTarget{
			Kind:    command.ReplyKind(row.ResponseKind),
			Topic:   nullStringValue(row.Topic),
			UserIDs: userIDs,
		},
	}
	if row.ChannelID.Valid {
		request.Target.ChannelID = row.ChannelID.Int64
	}
	return request, true, nil
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

// EmojiGroupMapping is the domain type for emoji -> group mappings.
type EmojiGroupMapping struct {
	ID             int64
	ShortName      string
	DisplayName    string
	ChannelGroupID int64
	EmojiName      string
	EmojiCode      string
	ReactionType   string
	Enabled        bool
	SortOrder      int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func emojiGroupMappingFromRow(row storagedb.EmojiGroupMapping) (EmojiGroupMapping, error) {
	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return EmojiGroupMapping{}, err
	}
	updatedAt, err := parseTime(row.UpdatedAt)
	if err != nil {
		return EmojiGroupMapping{}, err
	}
	return EmojiGroupMapping{
		ID:             row.ID,
		ShortName:      row.ShortName,
		DisplayName:    row.DisplayName,
		ChannelGroupID: row.ChannelGroupID,
		EmojiName:      row.EmojiName,
		EmojiCode:      row.EmojiCode,
		ReactionType:   row.ReactionType,
		Enabled:        row.Enabled != 0,
		SortOrder:      row.SortOrder,
		CreatedAt:      createdAt,
		UpdatedAt:      updatedAt,
	}, nil
}

// UpsertEmojiGroupMapping creates or updates an emoji->group mapping.
func (repo *Repository) UpsertEmojiGroupMapping(ctx context.Context, m EmojiGroupMapping) error {
	now := repo.now()
	createdAt := formatTime(now)
	if !m.CreatedAt.IsZero() {
		createdAt = formatTime(m.CreatedAt)
	}
	enabled := int64(0)
	if m.Enabled {
		enabled = 1
	}
	if err := repo.queries.UpsertEmojiGroupMapping(ctx, storagedb.UpsertEmojiGroupMappingParams{
		ShortName:      m.ShortName,
		DisplayName:    m.DisplayName,
		ChannelGroupID: m.ChannelGroupID,
		EmojiName:      m.EmojiName,
		EmojiCode:      m.EmojiCode,
		ReactionType:   m.ReactionType,
		Enabled:        enabled,
		SortOrder:      m.SortOrder,
		CreatedAt:      createdAt,
		UpdatedAt:      formatTime(now),
	}); err != nil {
		return fmt.Errorf("upsert emoji group mapping %q: %w", m.ShortName, err)
	}
	return nil
}

// ListEnabledEmojiGroupMappings lists enabled mappings ordered for announcement rendering.
func (repo *Repository) ListEnabledEmojiGroupMappings(ctx context.Context) ([]EmojiGroupMapping, error) {
	rows, err := repo.queries.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled emoji group mappings: %w", err)
	}
	result := make([]EmojiGroupMapping, 0, len(rows))
	for _, row := range rows {
		m, err := emojiGroupMappingFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}

// ListAllEmojiGroupMappings lists all mappings (for admin commands).
func (repo *Repository) ListAllEmojiGroupMappings(ctx context.Context) ([]EmojiGroupMapping, error) {
	rows, err := repo.queries.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all emoji group mappings: %w", err)
	}
	result := make([]EmojiGroupMapping, 0, len(rows))
	for _, row := range rows {
		m, err := emojiGroupMappingFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, nil
}

// GetEmojiGroupMappingByShortName looks up an enabled mapping by short name.
func (repo *Repository) GetEmojiGroupMappingByShortName(
	ctx context.Context,
	shortName string,
) (EmojiGroupMapping, bool, error) {
	row, err := repo.queries.GetEmojiGroupMappingByShortName(ctx, shortName)
	if errors.Is(err, sql.ErrNoRows) {
		return EmojiGroupMapping{}, false, nil
	}
	if err != nil {
		return EmojiGroupMapping{}, false, fmt.Errorf("get emoji group mapping by short name %q: %w", shortName, err)
	}
	m, err := emojiGroupMappingFromRow(row)
	if err != nil {
		return EmojiGroupMapping{}, false, err
	}
	return m, true, nil
}

// GetEmojiGroupMappingByEmoji looks up an enabled mapping by emoji identity.
func (repo *Repository) GetEmojiGroupMappingByEmoji(
	ctx context.Context,
	emojiName, reactionType string,
) (EmojiGroupMapping, bool, error) {
	row, err := repo.queries.GetEmojiGroupMappingByEmoji(ctx, storagedb.GetEmojiGroupMappingByEmojiParams{
		EmojiName:    emojiName,
		ReactionType: reactionType,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return EmojiGroupMapping{}, false, nil
	}
	if err != nil {
		return EmojiGroupMapping{}, false, fmt.Errorf("get emoji group mapping by emoji %q: %w", emojiName, err)
	}
	m, err := emojiGroupMappingFromRow(row)
	if err != nil {
		return EmojiGroupMapping{}, false, err
	}
	return m, true, nil
}

// SetEmojiGroupMappingEnabled enables or disables a mapping by short name.
func (repo *Repository) SetEmojiGroupMappingEnabled(ctx context.Context, shortName string, enabled bool) error {
	enabledInt := int64(0)
	if enabled {
		enabledInt = 1
	}
	if err := repo.queries.SetEmojiGroupMappingEnabled(ctx, storagedb.SetEmojiGroupMappingEnabledParams{
		Enabled:   enabledInt,
		UpdatedAt: formatTime(repo.now()),
		ShortName: shortName,
	}); err != nil {
		return fmt.Errorf("set emoji group mapping enabled for %q: %w", shortName, err)
	}
	return nil
}

// AnnouncementState holds the stored announcement message state.
type AnnouncementState struct {
	MessageID   *int64
	ContentHash string
	UpdatedAt   time.Time
}

// GetAnnouncementState returns the current announcement state (ok=false if not yet seeded).
func (repo *Repository) GetAnnouncementState(ctx context.Context) (AnnouncementState, bool, error) {
	row, err := repo.queries.GetAnnouncementState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return AnnouncementState{}, false, nil
	}
	if err != nil {
		return AnnouncementState{}, false, fmt.Errorf("get announcement state: %w", err)
	}
	updatedAt, err := parseTime(row.UpdatedAt)
	if err != nil {
		return AnnouncementState{}, false, err
	}
	state := AnnouncementState{
		ContentHash: row.ContentHash,
		UpdatedAt:   updatedAt,
	}
	if row.MessageID.Valid {
		id := row.MessageID.Int64
		state.MessageID = &id
	}
	return state, true, nil
}

// SaveAnnouncementState persists the announcement message state.
func (repo *Repository) SaveAnnouncementState(ctx context.Context, state AnnouncementState) error {
	var messageID sql.NullInt64
	if state.MessageID != nil {
		messageID = sql.NullInt64{Int64: *state.MessageID, Valid: true}
	}
	if err := repo.queries.SaveAnnouncementState(ctx, storagedb.SaveAnnouncementStateParams{
		MessageID:   messageID,
		ContentHash: state.ContentHash,
		UpdatedAt:   formatTime(repo.now()),
	}); err != nil {
		return fmt.Errorf("save announcement state: %w", err)
	}
	return nil
}

// IsReactionProcessed returns true if this reaction event has already been processed.
func (repo *Repository) IsReactionProcessed(
	ctx context.Context,
	messageID, userID int64,
	emojiName, op string,
) (bool, error) {
	_, err := repo.queries.IsReactionProcessed(ctx, storagedb.IsReactionProcessedParams{
		MessageID: messageID,
		UserID:    userID,
		EmojiName: emojiName,
		Op:        op,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check processed reaction: %w", err)
	}
	return true, nil
}

// MarkReactionProcessed records that a reaction event has been processed.
func (repo *Repository) MarkReactionProcessed(
	ctx context.Context,
	messageID, userID int64,
	emojiName, op string,
) error {
	if err := repo.queries.MarkReactionProcessed(ctx, storagedb.MarkReactionProcessedParams{
		MessageID:   messageID,
		UserID:      userID,
		EmojiName:   emojiName,
		Op:          op,
		ProcessedAt: formatTime(repo.now()),
	}); err != nil {
		return fmt.Errorf("mark reaction processed: %w", err)
	}
	return nil
}
