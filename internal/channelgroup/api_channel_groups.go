// Package channelgroup extends the upstream go-zulip client with endpoints
// for "channel groups": an application-level aggregate backed by one Zulip
// user group. Subscribing a user to a channel group subscribes them to every
// channel in the group and adds them to the backing user group.
//
// The API surface intentionally mirrors the shape of the upstream
// user-group endpoints (see zulip/api/users.APIUsers) so that callers
// already familiar with go-zulip can use it without context switching.
package channelgroup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/callorigin"
	channelgroupdb "github.com/tum-zulip/go-campusbot/internal/channelgroup/db"
	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	"github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

// Names of the high-level channel-group operations, propagated through ctx
// via [callorigin] so test doubles can distinguish which logical op issued a
// low-level call. Unused in production.
const (
	originCreate         = "CreateChannelGroup"
	originImport         = "ImportZulipUserGroup"
	originUpdateChannels = "UpdateChannelGroupChannels"
	originFolderAdd      = "AddChannelGroupFolder"
	originFolderRemove   = "RemoveChannelGroupFolder"
	originFolderAssign   = "AssignChannelGroupFolder"
	originFolderUnassign = "UnassignChannelGroupFolder"
	originSubscribe      = "SubscribeToChannelGroup"
	originUnsubscribe    = "UnsubscribeFromChannelGroup"
)

const zulipAdministratorsSystemGroupName = "role:administrators"

// Client is the campusbot Zulip client. It is a drop-in replacement for
// client.Client and additionally exposes channel-group endpoints.
type Client interface {
	client.Client
	APIChannelGroups
}

// channelGroupClient embeds the upstream client so every existing endpoint
// is forwarded unchanged, and composes the channel-groups service on top.
type channelGroupClient struct {
	client.Client
	APIChannelGroups
}

type ClientOption func(*channelGroups)

// WithLogger configures structured logging for channel-group operations.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(s *channelGroups) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithRunContext sets the context used for the background event-listener
// goroutine. Use this when the context passed to NewClient is a short-lived
// startup context: the goroutine must survive until the application shuts
// down, so it needs a context tied to the application lifetime, not to
// initialization.
func WithRunContext(ctx context.Context) ClientOption {
	return func(s *channelGroups) {
		if ctx != nil {
			s.runCtx = ctx
		}
	}
}

// NewClient wraps an upstream client, reconciles persisted channel-group
// metadata against Zulip state, and starts the channel-group event listener
// before returning it.
//
// Use [WithRunContext] to supply a long-lived context for the background
// event-listener goroutine when ctx is a short-lived startup context.
func NewClient(
	ctx context.Context,
	base client.Client,
	database *sql.DB,
	opts ...ClientOption,
) (Client, error) {
	service := newChannelGroups(base, database, opts...)
	if err := service.initializeChannelGroups(ctx); err != nil {
		return nil, err
	}
	if err := service.startChannelGroupEventListener(ctx); err != nil {
		return nil, err
	}
	return newClientWithService(base, service), nil
}

func newClientWithService(base client.Client, service APIChannelGroups) Client {
	return &channelGroupClient{
		Client:           base,
		APIChannelGroups: service,
	}
}

func (c *channelGroupClient) Close() error {
	if c == nil || c.APIChannelGroups == nil {
		return nil
	}
	closer, ok := c.APIChannelGroups.(interface{ Close() error })
	if !ok {
		return nil
	}
	return closer.Close()
}

type channelGroups struct {
	base    client.Client
	db      *sql.DB
	queries *channelgroupdb.Queries
	logger  *slog.Logger
	runCtx  context.Context // overrides the ctx passed to startChannelGroupEventListener

	folderOpsMu sync.Mutex

	listenerMu     sync.Mutex
	listenerCancel context.CancelFunc
	listenerWG     sync.WaitGroup
}

func newChannelGroups(base client.Client, database *sql.DB, opts ...ClientOption) *channelGroups {
	service := &channelGroups{
		base:    base,
		db:      database,
		queries: channelgroupdb.New(database),
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

var _ APIChannelGroups = (*channelGroups)(nil)

type ChannelFolderConflictError struct {
	ChannelID           int64
	ExpectedFolderID    int64
	ConflictingFolderID int64
}

func (err ChannelFolderConflictError) Error() string {
	return fmt.Sprintf(
		"channel %d is in channel folder %d, not managed channel folder %d",
		err.ChannelID,
		err.ConflictingFolderID,
		err.ExpectedFolderID,
	)
}

type ChannelFolderExternalChannelError struct {
	ChannelID       int64
	ChannelFolderID int64
}

func (err ChannelFolderExternalChannelError) Error() string {
	return fmt.Sprintf(
		"channel %d is in managed channel folder %d but not in the channel group",
		err.ChannelID,
		err.ChannelFolderID,
	)
}

func (s *channelGroups) withTx(ctx context.Context, fn func(*channelgroupdb.Queries) error) error {
	if s.db == nil {
		return errors.New("database connection must not be nil")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin channelgroup SQLite transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err := fn(s.queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit channelgroup SQLite transaction: %w", err)
	}
	tx = nil
	return nil
}

func (s *channelGroups) initializeChannelGroups(ctx context.Context) error {
	groups, err := s.getGroups(ctx)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}

	userGroups, err := s.userGroupsByID(ctx)
	if err != nil {
		return err
	}
	subscribedChannelIDs, err := s.subscribedChannelIDs(ctx)
	if err != nil {
		return err
	}

	for _, group := range groups {
		userGroup, ok := userGroups[group.ID]
		if !ok || userGroup.Deactivated {
			if err = s.deleteChannelGroup(ctx, group.ID); err != nil {
				return err
			}
			s.logger.InfoContext(
				ctx,
				"removed channel group with missing or deactivated user group",
				"channel_group_id",
				group.ID,
				"user_group_exists",
				ok,
			)
			continue
		}

		for _, channelID := range group.ChannelIDs {
			if _, ok = subscribedChannelIDs[channelID]; ok {
				continue
			}
			if err = s.removeChannelGroupChannel(ctx, group.ID, channelID); err != nil {
				return err
			}
			s.logger.InfoContext(ctx, "removed stale channel from channel group",
				"channel_group_id", group.ID,
				"channel_id", channelID,
			)
		}
	}

	return nil
}

func (s *channelGroups) startChannelGroupEventListener(ctx context.Context) error {
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	queueID, lastEventID, err := s.ensureChannelGroupEventQueue(ctx)
	if err != nil {
		return err
	}
	goroutineCtx := ctx
	if s.runCtx != nil {
		goroutineCtx = s.runCtx
	}
	listenerCtx, cancel := context.WithCancel(goroutineCtx)

	s.listenerMu.Lock()
	s.listenerCancel = cancel
	s.listenerWG.Add(1)
	s.listenerMu.Unlock()

	go func() {
		defer s.listenerWG.Done()
		s.runChannelGroupEventListener(listenerCtx, queueID, lastEventID)
	}()
	return nil
}

func (s *channelGroups) Close() error {
	if s == nil {
		return nil
	}

	s.listenerMu.Lock()
	cancel := s.listenerCancel
	s.listenerCancel = nil
	s.listenerMu.Unlock()

	if cancel != nil {
		cancel()
	}
	s.listenerWG.Wait()
	return nil
}

type channelGroupEventQueueState struct {
	QueueID     string
	LastEventID int64
}

func (s *channelGroups) channelGroupEventQueueState(
	ctx context.Context,
) (channelGroupEventQueueState, bool, error) {
	row, err := s.queries.GetChannelGroupEventQueueState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return channelGroupEventQueueState{}, false, nil
	}
	if err != nil {
		return channelGroupEventQueueState{}, false, fmt.Errorf(
			"read channel-group event queue state: %w",
			err,
		)
	}
	return channelGroupEventQueueState{
		QueueID:     row.QueueID,
		LastEventID: row.LastEventID,
	}, true, nil
}

func (s *channelGroups) saveChannelGroupEventQueueState(
	ctx context.Context,
	state channelGroupEventQueueState,
) error {
	if state.QueueID == "" {
		return errors.New("channel-group event queue ID must not be empty")
	}
	if err := s.queries.SaveChannelGroupEventQueueState(ctx, channelgroupdb.SaveChannelGroupEventQueueStateParams{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
		UpdatedAt:   formatChannelGroupEventQueueTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("save channel-group event queue state: %w", err)
	}
	return nil
}

func (s *channelGroups) ensureChannelGroupEventQueue(ctx context.Context) (string, int64, error) {
	stored, ok, err := s.channelGroupEventQueueState(ctx)
	if err != nil {
		return "", 0, err
	}
	if ok {
		s.logger.InfoContext(ctx, "resuming channel-group Zulip event queue",
			"queue_id", stored.QueueID,
			"last_event_id", stored.LastEventID)
		return stored.QueueID, stored.LastEventID, nil
	}
	return s.registerAndSaveChannelGroupEventQueue(ctx)
}

func (s *channelGroups) registerAndSaveChannelGroupEventQueue(
	ctx context.Context,
) (string, int64, error) {
	queueID, lastEventID, err := s.registerChannelGroupEventQueue(ctx)
	if err != nil {
		return "", 0, err
	}
	if err := s.saveChannelGroupEventQueueState(ctx, channelGroupEventQueueState{
		QueueID:     queueID,
		LastEventID: lastEventID,
	}); err != nil {
		return "", 0, err
	}
	s.logger.InfoContext(ctx, "registered channel-group Zulip event queue",
		"queue_id", queueID,
		"last_event_id", lastEventID)
	return queueID, lastEventID, nil
}

func (s *channelGroups) registerChannelGroupEventQueue(ctx context.Context) (string, int64, error) {
	resp, _, err := s.base.RegisterQueue(ctx).
		AllPublicChannels(true).
		ApplyMarkdown(false).
		EventTypes([]events.EventType{events.EventTypeChannel, events.EventTypeUserGroup}).
		ClientCapabilities(map[string]interface{}{
			"notification_settings_null": true,
			"include_deactivated_groups": false,
			"archived_channels":          true,
		}).
		Execute()
	if err != nil {
		return "", 0, err
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return "", 0, errors.New("register channel-group event queue: empty queue ID")
	}
	return *resp.QueueID, resp.LastEventID, nil
}

//nolint:funlen // event queue replacement flow is easier to follow without splitting retry state across helpers.
func (s *channelGroups) runChannelGroupEventListener(
	ctx context.Context,
	queueID string,
	lastEventID int64,
) {
	for {
		var retry bool
		var err error
		retry, lastEventID, err = s.consumeChannelGroupEventQueue(ctx, queueID, lastEventID)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !retry {
			s.logger.WarnContext(
				ctx,
				"failed to consume channel-group Zulip event queue",
				"error",
				err,
			)
			if !waitChannelGroupEventQueueRetry(ctx) {
				return
			}
			continue
		}
		if !retry {
			return
		}

		s.logger.WarnContext(
			ctx,
			"channel-group Zulip event queue expired; registering a new queue; events may have been missed",
			"queue_id",
			queueID,
			"last_event_id",
			lastEventID,
			"error",
			err,
		)
		if clearErr := s.queries.ClearChannelGroupEventQueueState(ctx); clearErr != nil {
			s.logger.WarnContext(
				ctx,
				"failed to clear expired channel-group event queue state",
				"error",
				clearErr,
			)
		}
		newQueueID, replacementLastEventID, registerErr := s.registerAndSaveChannelGroupEventQueue(
			ctx,
		)
		if registerErr != nil {
			s.logger.WarnContext(
				ctx,
				"failed to register replacement channel-group Zulip event queue",
				"error",
				registerErr,
			)
			if !waitChannelGroupEventQueueRetry(ctx) {
				return
			}
			continue
		}
		queueID = newQueueID
		lastEventID = replacementLastEventID
	}
}

func (s *channelGroups) consumeChannelGroupEventQueue(
	ctx context.Context,
	queueID string,
	lastEventID int64,
) (bool, int64, error) {
	errs := make(chan error, 1)
	queue := realtimeevents.NewEventQueue(
		s.base,
		realtimeevents.WithLogger(s.logger),
		realtimeevents.WithEventQueueChannelErrorHandler(s.logger, errs),
	)

	queueCtx, cancelQueue := context.WithCancel(ctx)
	defer cancelQueue()

	eventCh, err := queue.Connect(queueCtx, queueID, lastEventID)
	if err != nil {
		if isBadChannelGroupEventQueueID(err) {
			return true, lastEventID, err
		}
		return false, lastEventID, fmt.Errorf("connect to channel-group Zulip event queue: %w", err)
	}
	defer func() {
		if closeErr := queue.Close(); closeErr != nil {
			s.logger.WarnContext(
				ctx,
				"failed to close channel-group Zulip event queue",
				"error",
				closeErr,
			)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return false, lastEventID, nil
		case event, ok := <-eventCh:
			if !ok {
				return consumeClosedChannelGroupEventQueue(ctx, errs, lastEventID)
			}
			lastEventID = s.handlePolledChannelGroupEvent(ctx, queueID, event, lastEventID)
		case pollErr := <-errs:
			if isBadChannelGroupEventQueueID(pollErr) {
				return true, lastEventID, pollErr
			}
			s.logger.WarnContext(
				ctx,
				"failed to poll channel-group Zulip event queue",
				"error",
				pollErr,
			)
		}
	}
}

func consumeClosedChannelGroupEventQueue(
	ctx context.Context,
	errs <-chan error,
	lastEventID int64,
) (bool, int64, error) {
	select {
	case pollErr := <-errs:
		if isBadChannelGroupEventQueueID(pollErr) {
			return true, lastEventID, pollErr
		}
		return false, lastEventID, pollErr
	default:
		return false, lastEventID, ctx.Err()
	}
}

func (s *channelGroups) handlePolledChannelGroupEvent(
	ctx context.Context,
	queueID string,
	event events.Event,
	lastEventID int64,
) int64 {
	if event == nil {
		s.logger.WarnContext(ctx, "received nil channel-group Zulip event")
		return lastEventID
	}
	if err := s.handleChannelGroupEvent(ctx, event); err != nil {
		s.logger.WarnContext(ctx, "failed to process channel-group Zulip event",
			"event_id", event.GetID(),
			"error", err,
		)
		return lastEventID
	}

	lastEventID = event.GetID()
	if err := s.saveChannelGroupEventQueueState(ctx, channelGroupEventQueueState{
		QueueID:     queueID,
		LastEventID: lastEventID,
	}); err != nil {
		s.logger.WarnContext(ctx, "failed to persist channel-group Zulip event queue state",
			"queue_id", queueID,
			"last_event_id", lastEventID,
			"error", err,
		)
	}
	return lastEventID
}

func waitChannelGroupEventQueueRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func formatChannelGroupEventQueueTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func isBadChannelGroupEventQueueID(err error) bool {
	if err == nil {
		return false
	}
	var badQueue zulip.BadEventQueueIDError
	if errors.As(err, &badQueue) {
		return true
	}
	var coded zulip.CodedError
	return errors.As(err, &coded) && coded.Code == "BAD_EVENT_QUEUE_ID"
}

func (s *channelGroups) handleChannelGroupEvent(ctx context.Context, event events.Event) error {
	switch event := event.(type) {
	case events.ChannelDeleteEvent:
		for _, channelID := range channelDeleteEventIDs(event) {
			if err := s.removeChannelFromChannelGroups(ctx, channelID); err != nil {
				return err
			}
		}
	case events.ChannelUpdateEvent:
		if event.Property == "is_archived" && event.Value != nil && event.Value.Bool != nil && *event.Value.Bool {
			return s.removeChannelFromChannelGroups(ctx, event.ChannelID)
		}
	case events.UserGroupRemoveEvent:
		return s.removeDeletedUserGroupChannelGroup(ctx, event.GroupID)
	case events.UserGroupUpdateEvent:
		if event.Data.Deactivated != nil && *event.Data.Deactivated {
			return s.removeDeletedUserGroupChannelGroup(ctx, event.GroupID)
		}
	case *events.EventUnmarshalingError:
		s.logger.WarnContext(ctx, "failed to decode channel-group Zulip event",
			"event_id", event.GetID(),
			"event_type", event.Type,
			"error", event.Err,
		)
	}
	return nil
}

func channelDeleteEventIDs(event events.ChannelDeleteEvent) []int64 {
	if len(event.ChannelIDs) > 0 {
		return uniqueInt64s(event.ChannelIDs)
	}

	ids := make([]int64, 0, len(event.Channels))
	for _, channel := range event.Channels {
		fields, ok := channel.(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range []string{"stream_id", "id"} {
			value, ok := fields[key]
			if !ok {
				continue
			}
			switch value := value.(type) {
			case float64:
				ids = append(ids, int64(value))
			case int64:
				ids = append(ids, value)
			case int:
				ids = append(ids, int64(value))
			}
			break
		}
	}
	return uniqueInt64s(ids)
}

func (s *channelGroups) removeChannelFromChannelGroups(ctx context.Context, channelID int64) error {
	err := s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		return q.RemoveChannelFromChannelGroups(ctx, channelID)
	})
	if err != nil {
		return err
	}
	s.logger.InfoContext(
		ctx,
		"removed deleted channel from channel groups",
		"channel_id",
		channelID,
	)
	return nil
}

func (s *channelGroups) removeDeletedUserGroupChannelGroup(
	ctx context.Context,
	userGroupID int64,
) error {
	if err := s.deleteChannelGroup(ctx, userGroupID); err != nil {
		return err
	}
	s.logger.InfoContext(
		ctx,
		"removed channel group for deleted user group",
		"channel_group_id",
		userGroupID,
	)
	return nil
}

func (s *channelGroups) CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest {
	return CreateChannelGroupRequest{ctx: ctx, apiService: s}
}

//nolint:funlen,gocognit // request validation and provisioning rollback are kept together for atomicity
func (s *channelGroups) CreateChannelGroupExecute(
	r CreateChannelGroupRequest,
) (*CreateChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originCreate)
	if r.name == nil {
		return nil, nil, errors.New("name is required and must be specified")
	}

	initialMembers, err := userIDPrincipals(r.initialSubscribers)
	if err != nil {
		return nil, nil, err
	}

	group := ChannelGroup{
		Name:       *r.name,
		ChannelIDs: nil,
	}
	if r.channelIDs != nil {
		group.ChannelIDs = uniqueInt64s(*r.channelIDs)
	}

	adminsGroupSetting, err := s.administratorsGroupSetting(r.ctx)
	if err != nil {
		return nil, nil, err
	}

	userGroupResp, _, err := s.base.CreateUserGroup(r.ctx).
		Name(group.Name).
		Description("").
		Members(initialMembers).
		CanAddMembersGroup(adminsGroupSetting).
		CanJoinGroup(adminsGroupSetting).
		CanLeaveGroup(adminsGroupSetting).
		CanManageGroup(adminsGroupSetting).
		CanMentionGroup(adminsGroupSetting).
		CanRemoveMembersGroup(adminsGroupSetting).
		Execute()
	if err != nil {
		return nil, nil, err
	}
	group.ID = userGroupResp.GroupID
	createdUserGroup := true
	dbGroupCreated := false
	createdChannelFolderID := int64(0)
	rollback := func(cause error) error {
		var rollbackErrs []error
		if dbGroupCreated {
			if err := s.deleteChannelGroup(r.ctx, group.ID); err != nil {
				rollbackErrs = append(
					rollbackErrs,
					fmt.Errorf("delete local channel group %d: %w", group.ID, err),
				)
			}
		}
		if createdChannelFolderID != 0 {
			if err := s.rollbackCreatedChannelFolder(r.ctx, createdChannelFolderID, group.ChannelIDs); err != nil {
				rollbackErrs = append(
					rollbackErrs,
					fmt.Errorf("archive channel folder %d: %w", createdChannelFolderID, err),
				)
			}
		}
		if createdUserGroup {
			if _, _, err := s.base.DeactivateUserGroup(r.ctx, group.ID).Execute(); err != nil {
				rollbackErrs = append(
					rollbackErrs,
					fmt.Errorf("deactivate user group %d: %w", group.ID, err),
				)
			}
		}
		if len(rollbackErrs) == 0 {
			return cause
		}
		return fmt.Errorf("%w (rollback failed: %w)", cause, errors.Join(rollbackErrs...))
	}

	var channelFolderID sql.NullInt64
	if r.createChannelFolder {
		folderResp, _, err := s.base.CreateChannelFolder(r.ctx).
			Name(group.Name).
			Description("").
			Execute()
		if err != nil {
			return nil, nil, rollback(err)
		}
		channelFolderID = sql.NullInt64{Int64: folderResp.ChannelFolderID, Valid: true}
		createdChannelFolderID = folderResp.ChannelFolderID
		group.ChannelFolderID = &channelFolderID.Int64
	}

	if len(group.ChannelIDs) > 0 && len(initialMembers) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, initialMembers); err != nil {
			return nil, nil, rollback(err)
		}
	}
	if channelFolderID.Valid {
		if err = s.addChannelsToFolder(r.ctx, group.ChannelIDs, channelFolderID.Int64); err != nil {
			return nil, nil, rollback(err)
		}
	}

	var dbGroup channelgroupdb.ChannelGroup
	err = s.withTx(r.ctx, func(q *channelgroupdb.Queries) error {
		var err error
		dbGroup, err = q.CreateChannelGroup(r.ctx, channelgroupdb.CreateChannelGroupParams{
			ID:              group.ID,
			ChannelFolderID: channelFolderID,
		})
		if err != nil {
			return err
		}
		for _, channelID := range group.ChannelIDs {
			if err = q.AddChannelGroupChannel(r.ctx, channelgroupdb.AddChannelGroupChannelParams{
				ChannelGroupID: dbGroup.ID,
				ChannelID:      channelID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, rollback(err)
	}
	dbGroupID := dbGroup.ID
	dbGroupCreated = true

	s.logger.InfoContext(r.ctx, "created channel group",
		"channel_group_id", dbGroupID,
		"user_group_id", group.ID,
		"channel_folder_id", nullableInt64LogValue(channelFolderID),
		"name", group.Name,
		"channel_count", len(group.ChannelIDs),
		"subscriber_count", len(initialMembers),
	)
	return &CreateChannelGroupResponse{
		Response:       successResponse(),
		ChannelGroupID: dbGroupID,
	}, nil, nil
}

func (s *channelGroups) rollbackCreatedChannelFolder(
	ctx context.Context,
	channelFolderID int64,
	channelIDs []int64,
) error {
	var rollbackErrs []error
	if len(channelIDs) > 0 {
		if err := s.removeChannelsFromFolder(ctx, channelIDs, channelFolderID); err != nil {
			rollbackErrs = append(rollbackErrs, fmt.Errorf("remove channels from folder: %w", err))
		}
	}
	if err := s.archiveChannelFolder(ctx, channelFolderID); err != nil {
		rollbackErrs = append(rollbackErrs, err)
	}
	return errors.Join(rollbackErrs...)
}

// ImportZulipUserGroup records an existing Zulip user group as a local channel
// group. Unlike CreateChannelGroup it does NOT create a new Zulip user group;
// it only inserts the user-group ID into the local channel_groups table.
// The operation is idempotent: if the group is already tracked locally, it
// returns nil without touching the database.
func (s *channelGroups) ImportZulipUserGroup(ctx context.Context, userGroupID int64) error {
	ctx = callorigin.With(ctx, originImport)
	return s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		if _, err := q.GetChannelGroup(ctx, userGroupID); err == nil {
			return nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check channel group %d: %w", userGroupID, err)
		}
		if _, err := q.CreateChannelGroup(ctx, channelgroupdb.CreateChannelGroupParams{
			ID:              userGroupID,
			ChannelFolderID: sql.NullInt64{},
		}); err != nil {
			return fmt.Errorf("import channel group %d: %w", userGroupID, err)
		}
		return nil
	})
}

func (s *channelGroups) GetChannelGroups(ctx context.Context) GetChannelGroupsRequest {
	return GetChannelGroupsRequest{ctx: ctx, apiService: s}
}

func (s *channelGroups) GetChannelGroupsExecute(
	r GetChannelGroupsRequest,
) (*GetChannelGroupsResponse, *http.Response, error) {
	groups, err := s.getGroups(r.ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err = s.withUserGroupNames(r.ctx, groups)
	if err != nil {
		return nil, nil, err
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return &GetChannelGroupsResponse{
		Response:      successResponse(),
		ChannelGroups: groups,
	}, nil, nil
}

func (s *channelGroups) GetChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupRequest {
	return GetChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) GetChannelGroupExecute(
	r GetChannelGroupRequest,
) (*GetChannelGroupResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	group, err = s.withUserGroupName(r.ctx, group)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupResponse{
		Response:     successResponse(),
		ChannelGroup: group,
	}, nil, nil
}

func (s *channelGroups) DeleteChannelGroup(ctx context.Context, channelGroupID int64) error {
	if err := s.deleteChannelGroup(ctx, channelGroupID); err != nil {
		return fmt.Errorf("delete local channel group %d: %w", channelGroupID, err)
	}
	if _, _, err := s.base.DeactivateUserGroup(ctx, channelGroupID).Execute(); err != nil {
		return fmt.Errorf("deactivate user group %d: %w", channelGroupID, err)
	}
	return nil
}

func (s *channelGroups) GetChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupChannelsRequest {
	return GetChannelGroupChannelsRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) GetChannelGroupChannelsExecute(
	r GetChannelGroupChannelsRequest,
) (*GetChannelGroupChannelsResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupChannelsResponse{
		Response:   successResponse(),
		ChannelIDs: group.ChannelIDs,
	}, nil, nil
}

func (s *channelGroups) GetIsChannelInChannelGroup(
	ctx context.Context,
	channelGroupID int64,
	channelID int64,
) GetIsChannelInChannelGroupRequest {
	return GetIsChannelInChannelGroupRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
		channelID:      channelID,
	}
}

func (s *channelGroups) GetIsChannelInChannelGroupExecute(
	r GetIsChannelInChannelGroupRequest,
) (*GetIsChannelInChannelGroupResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelInChannelGroupResponse{
		Response:             successResponse(),
		IsChannelGroupMember: containsInt64(group.ChannelIDs, r.channelID),
	}, nil, nil
}

func (s *channelGroups) UpdateChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) UpdateChannelGroupChannelsRequest {
	return UpdateChannelGroupChannelsRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
	}
}

func (s *channelGroups) UpdateChannelGroupChannelsExecute(
	r UpdateChannelGroupChannelsRequest,
) (*zulip.Response, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originUpdateChannels)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	added := idsToAdd(group.ChannelIDs, r.addChannelIDs)
	deleted := idsToDelete(group.ChannelIDs, r.deleteChannelIDs)
	members, err := s.userGroupMembersForChannelUpdates(r.ctx, group.ID, added, deleted)
	if err != nil {
		return nil, nil, err
	}
	if len(added) > 0 && len(members) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, added, members); err != nil {
			return nil, nil, err
		}
	}
	if group.ChannelFolderID != nil {
		if err = s.addChannelsToFolder(r.ctx, added, *group.ChannelFolderID); err != nil {
			return nil, nil, err
		}
	}

	finalState, err := s.updateGroupChannels(
		r.ctx,
		r.channelGroupID,
		r.addChannelIDs,
		r.deleteChannelIDs,
	)
	if err != nil {
		return nil, nil, err
	}
	if err = s.cleanupAddedChannels(r.ctx, added, members, group.ID, finalState.ChannelIDs); err != nil {
		return nil, nil, err
	}

	s.logger.InfoContext(r.ctx, "updated channel group channels",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"added_channel_ids", added,
		"deleted_channel_ids", deleted,
		"channel_count", len(finalState.ChannelIDs),
		"subscriber_count", len(members),
	)
	return ptrResponse(successResponse()), nil, nil
}

func (s *channelGroups) UpdateChannelGroupFolder(
	ctx context.Context,
	channelGroupID int64,
) UpdateChannelGroupFolderRequest {
	return UpdateChannelGroupFolderRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) UpdateChannelGroupFolderExecute(
	r UpdateChannelGroupFolderRequest,
) (*zulip.Response, *http.Response, error) {
	if r.action == nil {
		return nil, nil, errors.New("folder action is required")
	}

	switch *r.action {
	case FolderActionAdd:
		r.ctx = callorigin.With(r.ctx, originFolderAdd)
	case FolderActionRemove:
		r.ctx = callorigin.With(r.ctx, originFolderRemove)
	case FolderActionAssign:
		r.ctx = callorigin.With(r.ctx, originFolderAssign)
	case FolderActionUnassign:
		r.ctx = callorigin.With(r.ctx, originFolderUnassign)
	default:
		return nil, nil, fmt.Errorf("unsupported folder action %q", *r.action)
	}

	s.folderOpsMu.Lock()
	defer s.folderOpsMu.Unlock()

	if err := s.updateChannelGroupFolder(r.ctx, r.channelGroupID, *r.action); err != nil {
		return nil, nil, err
	}
	return ptrResponse(successResponse()), nil, nil
}

func (s *channelGroups) updateChannelGroupFolder(
	ctx context.Context,
	channelGroupID int64,
	action FolderAction,
) error {
	group, err := s.getGroup(ctx, channelGroupID)
	if err != nil {
		return err
	}

	switch action {
	case FolderActionAdd:
		return s.addChannelGroupFolder(ctx, group)
	case FolderActionRemove:
		return s.removeChannelGroupFolder(ctx, group)
	case FolderActionAssign:
		return s.assignChannelGroupFolder(ctx, group)
	case FolderActionUnassign:
		return s.unassignChannelGroupFolder(ctx, group)
	default:
		return fmt.Errorf("unsupported folder action %q", action)
	}
}

func (s *channelGroups) addChannelGroupFolder(ctx context.Context, group ChannelGroup) error {
	if group.ChannelFolderID != nil {
		return s.assignChannelGroupFolder(ctx, group)
	}

	namedGroup, err := s.withUserGroupName(ctx, group)
	if err != nil {
		return err
	}
	folderResp, _, err := s.base.CreateChannelFolder(ctx).
		Name(namedGroup.Name).
		Description("").
		Execute()
	if err != nil {
		return err
	}

	folderID := folderResp.ChannelFolderID
	if err = s.setChannelGroupFolder(ctx, group.ID, sql.NullInt64{Int64: folderID, Valid: true}); err != nil {
		_, _, _ = s.base.UpdateChannelFolder(ctx, folderID).IsArchived(true).Execute()
		return err
	}

	group.ChannelFolderID = &folderID
	if err = s.assignChannelGroupFolder(ctx, group); err != nil {
		_ = s.setChannelGroupFolder(ctx, group.ID, sql.NullInt64{})
		_, _, _ = s.base.UpdateChannelFolder(ctx, folderID).IsArchived(true).Execute()
		return err
	}
	return nil
}

func (s *channelGroups) removeChannelGroupFolder(ctx context.Context, group ChannelGroup) error {
	if group.ChannelFolderID == nil {
		return nil
	}
	if err := s.rejectChannelsOutsideGroupInFolder(ctx, group.ChannelIDs, *group.ChannelFolderID); err != nil {
		return err
	}
	if err := s.removeChannelsFromFolder(ctx, group.ChannelIDs, *group.ChannelFolderID); err != nil {
		return err
	}
	if err := s.archiveChannelFolder(ctx, *group.ChannelFolderID); err != nil {
		return err
	}
	return s.setChannelGroupFolder(ctx, group.ID, sql.NullInt64{})
}

func (s *channelGroups) assignChannelGroupFolder(ctx context.Context, group ChannelGroup) error {
	if group.ChannelFolderID == nil {
		return errors.New("channel group has no channel folder")
	}
	return s.addChannelsToFolder(ctx, group.ChannelIDs, *group.ChannelFolderID)
}

func (s *channelGroups) unassignChannelGroupFolder(ctx context.Context, group ChannelGroup) error {
	if group.ChannelFolderID == nil {
		return nil
	}
	return s.removeChannelsFromFolder(ctx, group.ChannelIDs, *group.ChannelFolderID)
}

func (s *channelGroups) GetChannelGroupSubscribers(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupSubscribersRequest {
	return GetChannelGroupSubscribersRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
	}
}

func (s *channelGroups) GetChannelGroupSubscribersExecute(
	r GetChannelGroupSubscribersRequest,
) (*GetChannelGroupSubscribersResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	members, err := s.userGroupMembers(r.ctx, group.ID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupSubscribersResponse{
		Response:      successResponse(),
		SubscriberIDs: members,
	}, nil, nil
}

func (s *channelGroups) GetIsChannelGroupSubscriber(
	ctx context.Context,
	channelGroupID int64,
	userID int64,
) GetIsChannelGroupSubscriberRequest {
	return GetIsChannelGroupSubscriberRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
		userID:         userID,
	}
}

func (s *channelGroups) GetIsChannelGroupSubscriberExecute(
	r GetIsChannelGroupSubscriberRequest,
) (*GetIsChannelGroupSubscriberResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	resp, _, err := s.base.GetIsUserGroupMember(r.ctx, group.ID, r.userID).
		DirectMemberOnly(true).
		Execute()
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelGroupSubscriberResponse{
		Response:     successResponse(),
		IsSubscriber: resp.IsUserGroupMember,
	}, nil, nil
}

func (s *channelGroups) SubscribeToChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) SubscribeToChannelGroupRequest {
	return SubscribeToChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) SubscribeToChannelGroupExecute(
	r SubscribeToChannelGroupRequest,
) (*SubscribeToChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originSubscribe)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, group.ID).Add(userIDs).Execute()
	if err != nil {
		return nil, nil, err
	}

	latestState, touchedChannels, err := s.subscribeUsersToCurrentChannelGroupChannels(
		r.ctx,
		r.channelGroupID,
		userIDs,
	)
	if err != nil {
		_, _, _ = s.base.UpdateUserGroupMembers(r.ctx, group.ID).Delete(userIDs).Execute()
		_ = s.unsubscribeUsersFromChannels(r.ctx, touchedChannels, userIDs)
		return nil, nil, err
	}

	s.logger.InfoContext(r.ctx, "subscribed users to channel group",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"user_ids", userIDs,
		"channel_count", len(latestState.ChannelIDs),
	)
	return &SubscribeToChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *channelGroups) UnsubscribeFromChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) UnsubscribeFromChannelGroupRequest {
	return UnsubscribeFromChannelGroupRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
	}
}

//nolint:funlen // unsubscribe rollback/concurrency handling is easier to audit in one flow
func (s *channelGroups) UnsubscribeFromChannelGroupExecute(
	r UnsubscribeFromChannelGroupRequest,
) (*UnsubscribeFromChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originUnsubscribe)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	if !r.keepChannels && len(group.ChannelIDs) > 0 {
		if err = s.unsubscribeUsersFromChannelGroupChannels(
			r.ctx,
			r.channelGroupID,
			group.ChannelIDs,
			userIDs,
		); err != nil {
			return nil, nil, err
		}
	}
	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, group.ID).Delete(userIDs).Execute()
	if err != nil {
		if !r.keepChannels {
			_ = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, userIDs)
		}
		return nil, nil, err
	}
	finalState := group
	if !r.keepChannels {
		finalState, err = s.getGroup(r.ctx, r.channelGroupID)
		if err != nil {
			return nil, nil, err
		}
		addedWhileUnsubscribing := removeInt64s(finalState.ChannelIDs, group.ChannelIDs)
		if len(addedWhileUnsubscribing) > 0 {
			s.logger.DebugContext(
				r.ctx,
				"removing unsubscribed users from channels added concurrently",
				"channel_group_id",
				r.channelGroupID,
				"channel_ids",
				addedWhileUnsubscribing,
				"user_ids",
				userIDs,
			)
			if err = s.unsubscribeUsersFromChannelGroupChannels(
				r.ctx,
				r.channelGroupID,
				addedWhileUnsubscribing,
				userIDs,
			); err != nil {
				return nil, nil, err
			}
		}
	}

	s.logger.InfoContext(r.ctx, "unsubscribed users from channel group",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"user_ids", userIDs,
		"channel_count", len(finalState.ChannelIDs),
	)
	return &UnsubscribeFromChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *channelGroups) getGroup(ctx context.Context, channelGroupID int64) (ChannelGroup, error) {
	var group ChannelGroup
	err := s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		var err error
		group, err = getGroup(ctx, q, channelGroupID)
		return err
	})
	return group, err
}

func getGroup(
	ctx context.Context,
	q channelgroupdb.Querier,
	channelGroupID int64,
) (ChannelGroup, error) {
	dbGroup, err := q.GetChannelGroup(ctx, channelGroupID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelGroup{}, errChannelGroupNotFound(channelGroupID)
		}
		return ChannelGroup{}, err
	}
	channels, err := q.ListChannelGroupChannels(ctx, channelGroupID)
	if err != nil {
		return ChannelGroup{}, err
	}
	return channelGroupFromDB(dbGroup, channels), nil
}

func (s *channelGroups) getGroups(ctx context.Context) ([]ChannelGroup, error) {
	var groups []ChannelGroup
	err := s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		var err error
		groups, err = getGroups(ctx, q)
		return err
	})
	return groups, err
}

func getGroups(ctx context.Context, q channelgroupdb.Querier) ([]ChannelGroup, error) {
	dbGroups, err := q.ListChannelGroups(ctx)
	if err != nil {
		return nil, err
	}

	groups := make([]ChannelGroup, 0, len(dbGroups))
	for _, dbGroup := range dbGroups {
		var channels []int64
		channels, err = q.ListChannelGroupChannels(ctx, dbGroup.ID)
		if err != nil {
			return nil, err
		}
		groups = append(groups, channelGroupFromDB(dbGroup, channels))
	}
	return groups, nil
}

func (s *channelGroups) updateGroupChannels(
	ctx context.Context,
	channelGroupID int64,
	addChannelIDs *[]int64,
	deleteChannelIDs *[]int64,
) (ChannelGroup, error) {
	var group ChannelGroup
	err := s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		if _, err := getGroup(ctx, q, channelGroupID); err != nil {
			return err
		}
		if deleteChannelIDs != nil {
			for _, channelID := range uniqueInt64s(*deleteChannelIDs) {
				if err := q.RemoveChannelGroupChannel(ctx, channelgroupdb.RemoveChannelGroupChannelParams{
					ChannelGroupID: channelGroupID,
					ChannelID:      channelID,
				}); err != nil {
					return err
				}
			}
		}
		if addChannelIDs != nil {
			for _, channelID := range uniqueInt64s(*addChannelIDs) {
				if err := q.AddChannelGroupChannel(ctx, channelgroupdb.AddChannelGroupChannelParams{
					ChannelGroupID: channelGroupID,
					ChannelID:      channelID,
				}); err != nil {
					return err
				}
			}
		}
		var err error
		group, err = getGroup(ctx, q, channelGroupID)
		return err
	})
	return group, err
}

func (s *channelGroups) addChannelsToFolder(
	ctx context.Context,
	channelIDs []int64,
	channelFolderID int64,
) error {
	for _, channelID := range uniqueInt64s(channelIDs) {
		channelResp, _, err := s.base.GetChannelByID(ctx, channelID).Execute()
		if err != nil {
			return err
		}
		if channelResp.Channel.FolderID != nil {
			if *channelResp.Channel.FolderID == channelFolderID {
				continue
			}
			return ChannelFolderConflictError{
				ChannelID:           channelID,
				ExpectedFolderID:    channelFolderID,
				ConflictingFolderID: *channelResp.Channel.FolderID,
			}
		}
		if _, _, err := s.base.UpdateChannel(ctx, channelID).FolderID(channelFolderID).Execute(); err != nil {
			return err
		}
	}
	return nil
}

func (s *channelGroups) removeChannelsFromFolder(
	ctx context.Context,
	channelIDs []int64,
	channelFolderID int64,
) error {
	for _, channelID := range uniqueInt64s(channelIDs) {
		channelResp, _, err := s.base.GetChannelByID(ctx, channelID).Execute()
		if err != nil {
			return err
		}
		if channelResp.Channel.FolderID == nil {
			continue
		}
		if *channelResp.Channel.FolderID != channelFolderID {
			return ChannelFolderConflictError{
				ChannelID:           channelID,
				ExpectedFolderID:    channelFolderID,
				ConflictingFolderID: *channelResp.Channel.FolderID,
			}
		}
		if _, _, err = s.base.UpdateChannel(ctx, channelID).FolderIDNone().Execute(); err != nil {
			return err
		}
	}
	return nil
}

func (s *channelGroups) rejectChannelsOutsideGroupInFolder(
	ctx context.Context,
	groupChannelIDs []int64,
	channelFolderID int64,
) error {
	channelsResp, _, err := s.base.GetChannels(ctx).IncludeAll(true).Execute()
	if err != nil {
		return err
	}
	for _, channel := range channelsResp.Channels {
		if channel.FolderID == nil || *channel.FolderID != channelFolderID {
			continue
		}
		if !containsInt64(groupChannelIDs, channel.ChannelID) {
			return ChannelFolderExternalChannelError{
				ChannelID:       channel.ChannelID,
				ChannelFolderID: channelFolderID,
			}
		}
	}
	return nil
}

func (s *channelGroups) archiveChannelFolder(ctx context.Context, channelFolderID int64) error {
	_, _, err := s.base.UpdateChannelFolder(ctx, channelFolderID).IsArchived(true).Execute()
	var codedErr zulip.CodedError
	if errors.As(err, &codedErr) &&
		codedErr.Code == "BAD_REQUEST" &&
		strings.Contains(codedErr.Msg, "remove all the channels from this folder") {
		return ChannelFolderExternalChannelError{ChannelFolderID: channelFolderID}
	}
	return err
}

func (s *channelGroups) setChannelGroupFolder(
	ctx context.Context,
	channelGroupID int64,
	channelFolderID sql.NullInt64,
) error {
	return s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		return q.UpdateChannelGroupChannelFolder(
			ctx,
			channelgroupdb.UpdateChannelGroupChannelFolderParams{
				ID:              channelGroupID,
				ChannelFolderID: channelFolderID,
			},
		)
	})
}

func (s *channelGroups) deleteChannelGroup(ctx context.Context, channelGroupID int64) error {
	return s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		return q.DeleteChannelGroup(ctx, channelGroupID)
	})
}

func (s *channelGroups) removeChannelGroupChannel(
	ctx context.Context,
	channelGroupID int64,
	channelID int64,
) error {
	return s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		return q.RemoveChannelGroupChannel(ctx, channelgroupdb.RemoveChannelGroupChannelParams{
			ChannelGroupID: channelGroupID,
			ChannelID:      channelID,
		})
	})
}

func (s *channelGroups) cleanupAddedChannels(
	ctx context.Context,
	addedChannelIDs []int64,
	previousUserIDs []int64,
	userGroupID int64,
	currentChannelIDs []int64,
) error {
	channelsToClean := idsToDelete(currentChannelIDs, &addedChannelIDs)
	if len(channelsToClean) == 0 || len(previousUserIDs) == 0 {
		return nil
	}
	currentUserIDs, err := s.userGroupMembers(ctx, userGroupID)
	if err != nil {
		return err
	}
	staleUserIDs := removeInt64s(previousUserIDs, currentUserIDs)
	if len(staleUserIDs) == 0 {
		return nil
	}
	s.logger.DebugContext(ctx, "cleaning channels added during channel group membership change",
		"channel_ids", channelsToClean,
		"user_ids", staleUserIDs,
	)
	return s.unsubscribeUsersFromChannels(ctx, channelsToClean, staleUserIDs)
}

func (s *channelGroups) subscribeUsersToCurrentChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
	userIDs []int64,
) (ChannelGroup, []int64, error) {
	var group ChannelGroup
	activeUserIDs := userIDs
	touched := []int64{}
	attempted := map[int64]struct{}{}

	for {
		var err error
		group, err = s.getGroup(ctx, channelGroupID)
		if err != nil {
			return ChannelGroup{}, touched, err
		}

		channelIDs := removeAttemptedInt64s(group.ChannelIDs, attempted)
		if len(channelIDs) == 0 {
			return group, touched, nil
		}

		s.logger.DebugContext(ctx, "subscribing users to channel group channels",
			"channel_group_id", channelGroupID,
			"channel_ids", channelIDs,
			"user_ids", activeUserIDs,
		)
		if err = s.subscribeUsersToChannels(ctx, channelIDs, activeUserIDs); err != nil {
			return ChannelGroup{}, touched, err
		}
		touched = uniqueInt64s(append(touched, channelIDs...))
		for _, channelID := range channelIDs {
			attempted[channelID] = struct{}{}
		}

		currentUserIDs, err := s.userGroupMembers(ctx, group.ID)
		if err != nil {
			return ChannelGroup{}, touched, err
		}
		staleUserIDs := removeInt64s(activeUserIDs, currentUserIDs)
		if len(staleUserIDs) == 0 {
			continue
		}
		s.logger.DebugContext(ctx, "cleaning users removed during channel group subscribe",
			"channel_group_id", channelGroupID,
			"channel_ids", touched,
			"user_ids", staleUserIDs,
		)
		if err = s.unsubscribeUsersFromChannels(ctx, touched, staleUserIDs); err != nil {
			return ChannelGroup{}, touched, err
		}
		activeUserIDs = removeInt64s(activeUserIDs, staleUserIDs)
		if len(activeUserIDs) == 0 {
			return group, touched, nil
		}
	}
}

func (s *channelGroups) subscribeUsersToChannels(
	ctx context.Context,
	channelIDs []int64,
	userIDs []int64,
) error {
	subscriptions, err := s.subscriptionRequests(ctx, channelIDs)
	if err != nil {
		return err
	}
	_, _, err = s.base.Subscribe(ctx).
		Subscriptions(subscriptions).
		Principals(zulip.UserIDsAsPrincipals(userIDs...)).
		Execute()
	return err
}

func (s *channelGroups) unsubscribeUsersFromChannels(
	ctx context.Context,
	channelIDs []int64,
	userIDs []int64,
) error {
	channelNames, err := s.channelNames(ctx, channelIDs)
	if err != nil {
		return err
	}
	_, _, err = s.base.Unsubscribe(ctx).
		Subscriptions(channelNames).
		Principals(zulip.UserIDsAsPrincipals(userIDs...)).
		Execute()
	return err
}

//nolint:gocognit,funlen // the logic is lengthy but straightforward (hopefully)
func (s *channelGroups) unsubscribeUsersFromChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
	channelIDs []int64,
	userIDs []int64,
) error {
	channelIDs = uniqueInt64s(channelIDs)
	userIDs = uniqueInt64s(userIDs)
	if len(channelIDs) == 0 || len(userIDs) == 0 {
		return nil
	}

	var otherGroupChannels []channelgroupdb.ListOtherChannelGroupsForChannelsInGroupRow
	err := s.withTx(ctx, func(q *channelgroupdb.Queries) error {
		var err error
		otherGroupChannels, err = q.ListOtherChannelGroupsForChannelsInGroup(ctx, channelGroupID)
		return err
	})
	if err != nil {
		return err
	}
	if len(otherGroupChannels) == 0 {
		return s.unsubscribeUsersFromChannels(ctx, channelIDs, userIDs)
	}

	targetChannels := make(map[int64]struct{}, len(channelIDs))
	for _, channelID := range channelIDs {
		targetChannels[channelID] = struct{}{}
	}
	sharedChannelsByGroup := make(map[int64][]int64)
	for _, row := range otherGroupChannels {
		if _, ok := targetChannels[row.ChannelID]; !ok {
			continue
		}
		sharedChannelsByGroup[row.ChannelGroupID] = append(
			sharedChannelsByGroup[row.ChannelGroupID],
			row.ChannelID,
		)
	}
	if len(sharedChannelsByGroup) == 0 {
		return s.unsubscribeUsersFromChannels(ctx, channelIDs, userIDs)
	}

	coveredChannelsByUser := make(map[int64][]int64, len(userIDs))
	for otherGroupID, sharedChannelIDs := range sharedChannelsByGroup {
		members, err := s.userGroupMembers(ctx, otherGroupID)
		if err != nil {
			return err
		}
		for _, userID := range userIDs {
			if containsInt64(members, userID) {
				coveredChannelsByUser[userID] = append(
					coveredChannelsByUser[userID],
					sharedChannelIDs...)
			}
		}
	}

	unsubscribeUsersByChannels := make(map[string][]int64)
	unsubscribeChannelsByKey := make(map[string][]int64)
	for _, userID := range userIDs {
		unsubscribeChannelIDs := removeInt64s(channelIDs, coveredChannelsByUser[userID])
		if len(unsubscribeChannelIDs) == 0 {
			continue
		}
		key := fmt.Sprint(unsubscribeChannelIDs)
		unsubscribeChannelsByKey[key] = unsubscribeChannelIDs
		unsubscribeUsersByChannels[key] = append(unsubscribeUsersByChannels[key], userID)
	}

	keys := make([]string, 0, len(unsubscribeUsersByChannels))
	for key := range unsubscribeUsersByChannels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		unsubUserIDs := unsubscribeUsersByChannels[key]
		if err := s.unsubscribeUsersFromChannels(ctx, unsubscribeChannelsByKey[key], unsubUserIDs); err != nil {
			return err
		}
	}
	return nil
}

func (s *channelGroups) userGroupMembersForChannelUpdates(
	ctx context.Context,
	userGroupID int64,
	added []int64,
	deleted []int64,
) ([]int64, error) {
	if len(added) == 0 && len(deleted) == 0 {
		return nil, nil
	}
	return s.userGroupMembers(ctx, userGroupID)
}

func (s *channelGroups) subscriptionRequests(
	ctx context.Context,
	channelIDs []int64,
) ([]channels.SubscriptionRequest, error) {
	channelNames, err := s.channelNames(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	subscriptions := make([]channels.SubscriptionRequest, 0, len(channelNames))
	for _, name := range channelNames {
		subscriptions = append(subscriptions, channels.SubscriptionRequest{Name: name})
	}
	return subscriptions, nil
}

func (s *channelGroups) channelNames(ctx context.Context, channelIDs []int64) ([]string, error) {
	names := make([]string, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		resp, _, err := s.base.GetChannelByID(ctx, channelID).Execute()
		if err != nil {
			return nil, err
		}
		names = append(names, resp.Channel.Name)
	}
	return names, nil
}

func (s *channelGroups) userGroupMembers(ctx context.Context, userGroupID int64) ([]int64, error) {
	resp, _, err := s.base.GetUserGroupMembers(ctx, userGroupID).DirectMemberOnly(true).Execute()
	if err != nil {
		return nil, err
	}
	return uniqueInt64s(resp.Members), nil
}

func (s *channelGroups) userGroupsByID(ctx context.Context) (map[int64]zulip.UserGroup, error) {
	resp, _, err := s.base.GetUserGroups(ctx).IncludeDeactivatedGroups(false).Execute()
	if err != nil {
		return nil, err
	}
	groups := make(map[int64]zulip.UserGroup, len(resp.UserGroups))
	for _, group := range resp.UserGroups {
		groups[group.ID] = group
	}
	return groups, nil
}

func (s *channelGroups) subscribedChannelIDs(ctx context.Context) (map[int64]struct{}, error) {
	resp, _, err := s.base.GetSubscriptions(ctx).Execute()
	if err != nil {
		return nil, err
	}
	channelIDs := make(map[int64]struct{}, len(resp.Subscriptions))
	for _, subscription := range resp.Subscriptions {
		channelIDs[subscription.ChannelID] = struct{}{}
	}
	return channelIDs, nil
}

func (s *channelGroups) withUserGroupName(
	ctx context.Context,
	group ChannelGroup,
) (ChannelGroup, error) {
	groups, err := s.withUserGroupNames(ctx, []ChannelGroup{group})
	if err != nil {
		return ChannelGroup{}, err
	}
	return groups[0], nil
}

func (s *channelGroups) withUserGroupNames(
	ctx context.Context,
	groups []ChannelGroup,
) ([]ChannelGroup, error) {
	if len(groups) == 0 {
		return groups, nil
	}
	resp, _, err := s.base.GetUserGroups(ctx).IncludeDeactivatedGroups(true).Execute()
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(resp.UserGroups))
	for _, userGroup := range resp.UserGroups {
		names[userGroup.ID] = userGroup.Name
	}
	hydrated := make([]ChannelGroup, 0, len(groups))
	for _, group := range groups {
		name, ok := names[group.ID]
		if !ok {
			return nil, errChannelGroupNotFound(group.ID)
		}
		group = cloneChannelGroup(group)
		group.Name = name
		hydrated = append(hydrated, group)
	}
	return hydrated, nil
}

func channelGroupFromDB(dbGroup channelgroupdb.ChannelGroup, channelIDs []int64) ChannelGroup {
	group := ChannelGroup{
		ID:         dbGroup.ID,
		ChannelIDs: uniqueInt64s(channelIDs),
	}
	if dbGroup.ChannelFolderID.Valid {
		group.ChannelFolderID = &dbGroup.ChannelFolderID.Int64
	}
	return group
}

func nullableInt64LogValue(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}

func successResponse() zulip.Response {
	return zulip.Response{Result: zulip.ResponseSuccess}
}

func ptrResponse(r zulip.Response) *zulip.Response {
	return &r
}

// ErrChannelGroupNotFound is returned (wrapped) when a channel group ID does not
// exist in the local channelgroup database. Callers can detect it with errors.Is.
var ErrChannelGroupNotFound = errors.New("channel group not found")

func errChannelGroupNotFound(channelGroupID int64) error {
	return fmt.Errorf("channel group %d: %w", channelGroupID, ErrChannelGroupNotFound)
}

func cloneChannelGroup(group ChannelGroup) ChannelGroup {
	group.ChannelIDs = append([]int64(nil), group.ChannelIDs...)
	return group
}

func (s *channelGroups) administratorsGroupSetting(
	ctx context.Context,
) (zulip.GroupSettingValue, error) {
	resp, _, err := s.base.GetUserGroups(ctx).IncludeDeactivatedGroups(false).Execute()
	if err != nil {
		return zulip.GroupSettingValue{}, fmt.Errorf(
			"resolve Zulip administrators system group: %w",
			err,
		)
	}
	if resp == nil {
		return zulip.GroupSettingValue{}, errors.New(
			"resolve Zulip administrators system group: empty response",
		)
	}
	for _, group := range resp.UserGroups {
		if group.IsSystemGroup && group.Name == zulipAdministratorsSystemGroupName &&
			!group.Deactivated {
			groupID := group.ID
			return zulip.GroupSettingValue{GroupID: &groupID}, nil
		}
	}
	return zulip.GroupSettingValue{}, fmt.Errorf(
		"resolve Zulip administrators system group: %s not found",
		zulipAdministratorsSystemGroupName,
	)
}

func userIDPrincipals(principals *zulip.Principals) ([]int64, error) {
	if principals == nil {
		return []int64{}, nil
	}
	if principals.UserEmails != nil && len(*principals.UserEmails) > 0 {
		return nil, errors.New("channel group operations only support user ID principals")
	}
	if principals.UserIDs == nil {
		return []int64{}, nil
	}
	return uniqueInt64s(*principals.UserIDs), nil
}

func idsToAdd(existing []int64, requested *[]int64) []int64 {
	if requested == nil {
		return nil
	}
	added := make([]int64, 0, len(*requested))
	for _, id := range uniqueInt64s(*requested) {
		if !containsInt64(existing, id) {
			added = append(added, id)
		}
	}
	return added
}

func idsToDelete(existing []int64, requested *[]int64) []int64 {
	if requested == nil {
		return nil
	}
	deleted := make([]int64, 0, len(*requested))
	for _, id := range uniqueInt64s(*requested) {
		if containsInt64(existing, id) {
			deleted = append(deleted, id)
		}
	}
	return deleted
}

func uniqueInt64s(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	unique := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

func removeInt64s(values []int64, remove []int64) []int64 {
	removeSet := make(map[int64]struct{}, len(remove))
	for _, value := range remove {
		removeSet[value] = struct{}{}
	}
	remaining := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := removeSet[value]; !ok {
			remaining = append(remaining, value)
		}
	}
	return uniqueInt64s(remaining)
}

func removeAttemptedInt64s(values []int64, attempted map[int64]struct{}) []int64 {
	remaining := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := attempted[value]; !ok {
			remaining = append(remaining, value)
		}
	}
	return uniqueInt64s(remaining)
}

func containsInt64(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// APIChannelGroups is the set of endpoints provided by the channel-group
// service. Method shape (Builder / Execute split, context on the
// constructor) matches the upstream go-zulip conventions.
type APIChannelGroups interface {
	// CreateChannelGroup creates a new channel group, optionally
	// pre-populated with channels and initial subscribers.
	CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest
	CreateChannelGroupExecute(
		r CreateChannelGroupRequest,
	) (*CreateChannelGroupResponse, *http.Response, error)

	// ImportZulipUserGroup records an existing Zulip user group as a local
	// channel group, without creating a new Zulip user group. Idempotent.
	ImportZulipUserGroup(ctx context.Context, userGroupID int64) error

	// GetChannelGroups lists channel groups visible to the acting user.
	GetChannelGroups(ctx context.Context) GetChannelGroupsRequest
	GetChannelGroupsExecute(
		r GetChannelGroupsRequest,
	) (*GetChannelGroupsResponse, *http.Response, error)

	// GetChannelGroup fetches a single channel group by ID.
	GetChannelGroup(ctx context.Context, channelGroupID int64) GetChannelGroupRequest
	GetChannelGroupExecute(
		r GetChannelGroupRequest,
	) (*GetChannelGroupResponse, *http.Response, error)
	DeleteChannelGroup(ctx context.Context, channelGroupID int64) error

	// --- Channel membership inside a group --------------------------------
	// "Members" of a channel group are channels. Subscribers are tracked
	// in the backing Zulip user group.

	GetChannelGroupChannels(
		ctx context.Context,
		channelGroupID int64,
	) GetChannelGroupChannelsRequest
	GetChannelGroupChannelsExecute(
		r GetChannelGroupChannelsRequest,
	) (*GetChannelGroupChannelsResponse, *http.Response, error)

	GetIsChannelInChannelGroup(
		ctx context.Context,
		channelGroupID int64,
		channelID int64,
	) GetIsChannelInChannelGroupRequest
	GetIsChannelInChannelGroupExecute(
		r GetIsChannelInChannelGroupRequest,
	) (*GetIsChannelInChannelGroupResponse, *http.Response, error)

	// UpdateChannelGroupChannels adds and/or removes channels in a single
	// operation.
	UpdateChannelGroupChannels(
		ctx context.Context,
		channelGroupID int64,
	) UpdateChannelGroupChannelsRequest
	UpdateChannelGroupChannelsExecute(
		r UpdateChannelGroupChannelsRequest,
	) (*zulip.Response, *http.Response, error)

	// UpdateChannelGroupFolder creates, removes, assigns, or unassigns the
	// Zulip channel folder associated with a channel group.
	UpdateChannelGroupFolder(
		ctx context.Context,
		channelGroupID int64,
	) UpdateChannelGroupFolderRequest
	UpdateChannelGroupFolderExecute(
		r UpdateChannelGroupFolderRequest,
	) (*zulip.Response, *http.Response, error)

	// --- Subscribers ------------------------------------------------------
	// Subscribing a user (principal) to a channel group materializes
	// subscriptions to every channel currently in the group. Unsubscribing
	// removes them from every channel in the group.

	GetChannelGroupSubscribers(
		ctx context.Context,
		channelGroupID int64,
	) GetChannelGroupSubscribersRequest
	GetChannelGroupSubscribersExecute(
		r GetChannelGroupSubscribersRequest,
	) (*GetChannelGroupSubscribersResponse, *http.Response, error)

	GetIsChannelGroupSubscriber(
		ctx context.Context,
		channelGroupID int64,
		userID int64,
	) GetIsChannelGroupSubscriberRequest
	GetIsChannelGroupSubscriberExecute(
		r GetIsChannelGroupSubscriberRequest,
	) (*GetIsChannelGroupSubscriberResponse, *http.Response, error)

	SubscribeToChannelGroup(
		ctx context.Context,
		channelGroupID int64,
	) SubscribeToChannelGroupRequest
	SubscribeToChannelGroupExecute(
		r SubscribeToChannelGroupRequest,
	) (*SubscribeToChannelGroupResponse, *http.Response, error)

	UnsubscribeFromChannelGroup(
		ctx context.Context,
		channelGroupID int64,
	) UnsubscribeFromChannelGroupRequest
	UnsubscribeFromChannelGroupExecute(
		r UnsubscribeFromChannelGroupRequest,
	) (*UnsubscribeFromChannelGroupResponse, *http.Response, error)
}

// =============================================================================
// Domain model
// =============================================================================

// ChannelGroup is the wire representation of a channel group as returned by
// the server. Field names follow the conventions used by zulip.UserGroup.
type ChannelGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// ChannelIDs are the channels currently in the group.
	ChannelIDs []int64 `json:"channel_ids"`

	// ChannelFolderID is set when this channel group also manages a Zulip
	// channel folder.
	ChannelFolderID *int64 `json:"channel_folder_id,omitempty"`
}

// =============================================================================
// Request builders
//
// Only the *shape* of the builders is declared here. Each builder is expected
// to expose: chainable setters that return the builder by value, and an
// Execute() method that delegates to the matching ...Execute on the service.
// =============================================================================

// --- Create -----------------------------------------------------------------

type CreateChannelGroupRequest struct {
	ctx                 context.Context
	apiService          APIChannelGroups
	name                *string
	channelIDs          *[]int64
	initialSubscribers  *zulip.Principals
	createChannelFolder bool
}

func (r CreateChannelGroupRequest) Name(name string) CreateChannelGroupRequest {
	r.name = &name
	return r
}

func (r CreateChannelGroupRequest) ChannelIDs(ids []int64) CreateChannelGroupRequest {
	r.channelIDs = &ids
	return r
}

func (r CreateChannelGroupRequest) InitialSubscribers(
	p zulip.Principals,
) CreateChannelGroupRequest {
	r.initialSubscribers = &p
	return r
}

func (r CreateChannelGroupRequest) CreateChannelFolder(create bool) CreateChannelGroupRequest {
	r.createChannelFolder = create
	return r
}

func (r CreateChannelGroupRequest) Execute() (*CreateChannelGroupResponse, *http.Response, error) {
	return r.apiService.CreateChannelGroupExecute(r)
}

type CreateChannelGroupResponse struct {
	zulip.Response

	ChannelGroupID int64 `json:"channel_group_id"`
}

// --- Read -------------------------------------------------------------------

type GetChannelGroupsRequest struct {
	ctx        context.Context
	apiService APIChannelGroups
}

func (r GetChannelGroupsRequest) Execute() (*GetChannelGroupsResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupsExecute(r)
}

type GetChannelGroupsResponse struct {
	zulip.Response

	ChannelGroups []ChannelGroup `json:"channel_groups"`
}

type GetChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupRequest) Execute() (*GetChannelGroupResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupExecute(r)
}

type GetChannelGroupResponse struct {
	zulip.Response

	ChannelGroup ChannelGroup `json:"channel_group"`
}

// --- Channels in a group ----------------------------------------------------

type GetChannelGroupChannelsRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupChannelsRequest) Execute() (*GetChannelGroupChannelsResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupChannelsExecute(r)
}

type GetChannelGroupChannelsResponse struct {
	zulip.Response

	ChannelIDs []int64 `json:"channel_ids"`
}

type GetIsChannelInChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	channelID      int64
}

func (r GetIsChannelInChannelGroupRequest) Execute() (*GetIsChannelInChannelGroupResponse, *http.Response, error) {
	return r.apiService.GetIsChannelInChannelGroupExecute(r)
}

type GetIsChannelInChannelGroupResponse struct {
	zulip.Response

	IsChannelGroupMember bool `json:"is_channel_group_member"`
}

type UpdateChannelGroupChannelsRequest struct {
	ctx              context.Context
	apiService       APIChannelGroups
	channelGroupID   int64
	addChannelIDs    *[]int64
	deleteChannelIDs *[]int64
}

// Add specifies channels to add to the group. The server is expected to
// subscribe every current group subscriber to each added channel.
func (r UpdateChannelGroupChannelsRequest) Add(
	channelIDs []int64,
) UpdateChannelGroupChannelsRequest {
	r.addChannelIDs = &channelIDs
	return r
}

// Delete specifies channels to remove from the group. The server is expected
// to unsubscribe every current group subscriber from each removed channel
// (unless they are subscribed to it via another path).
func (r UpdateChannelGroupChannelsRequest) Delete(
	channelIDs []int64,
) UpdateChannelGroupChannelsRequest {
	r.deleteChannelIDs = &channelIDs
	return r
}

func (r UpdateChannelGroupChannelsRequest) Execute() (*zulip.Response, *http.Response, error) {
	return r.apiService.UpdateChannelGroupChannelsExecute(r)
}

type FolderAction string

const (
	FolderActionAdd      FolderAction = "add"
	FolderActionRemove   FolderAction = "remove"
	FolderActionAssign   FolderAction = "assign"
	FolderActionUnassign FolderAction = "unassign"
)

type UpdateChannelGroupFolderRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	action         *FolderAction
}

func (r UpdateChannelGroupFolderRequest) Add() UpdateChannelGroupFolderRequest {
	action := FolderActionAdd
	r.action = &action
	return r
}

func (r UpdateChannelGroupFolderRequest) Remove() UpdateChannelGroupFolderRequest {
	action := FolderActionRemove
	r.action = &action
	return r
}

func (r UpdateChannelGroupFolderRequest) Assign() UpdateChannelGroupFolderRequest {
	action := FolderActionAssign
	r.action = &action
	return r
}

func (r UpdateChannelGroupFolderRequest) Unassign() UpdateChannelGroupFolderRequest {
	action := FolderActionUnassign
	r.action = &action
	return r
}

func (r UpdateChannelGroupFolderRequest) Execute() (*zulip.Response, *http.Response, error) {
	return r.apiService.UpdateChannelGroupFolderExecute(r)
}

// --- Subscribers ------------------------------------------------------------

type GetChannelGroupSubscribersRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupSubscribersRequest) Execute() (*GetChannelGroupSubscribersResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupSubscribersExecute(r)
}

type GetChannelGroupSubscribersResponse struct {
	zulip.Response

	SubscriberIDs []int64 `json:"subscriber_ids"`
}

type GetIsChannelGroupSubscriberRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	userID         int64
}

func (r GetIsChannelGroupSubscriberRequest) Execute() (*GetIsChannelGroupSubscriberResponse, *http.Response, error) {
	return r.apiService.GetIsChannelGroupSubscriberExecute(r)
}

type GetIsChannelGroupSubscriberResponse struct {
	zulip.Response

	IsSubscriber bool `json:"is_subscriber"`
}

type SubscribeToChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	principals     *zulip.Principals
}

// Principals selects which users to subscribe. This implementation requires
// user ID principals so it can update the backing Zulip user group.
func (r SubscribeToChannelGroupRequest) Principals(
	p zulip.Principals,
) SubscribeToChannelGroupRequest {
	r.principals = &p
	return r
}

func (r SubscribeToChannelGroupRequest) Execute() (*SubscribeToChannelGroupResponse, *http.Response, error) {
	return r.apiService.SubscribeToChannelGroupExecute(r)
}

type SubscribeToChannelGroupResponse struct {
	zulip.Response
}

type UnsubscribeFromChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	principals     *zulip.Principals
	keepChannels   bool
}

// Principals selects which users to unsubscribe. This implementation requires
// user ID principals so it can update the backing Zulip user group.
func (r UnsubscribeFromChannelGroupRequest) Principals(
	p zulip.Principals,
) UnsubscribeFromChannelGroupRequest {
	r.principals = &p
	return r
}

// KeepChannels marks the request to only remove the user from the Zulip user
// group but not unsubscribe them from the group's channels.
func (r UnsubscribeFromChannelGroupRequest) KeepChannels() UnsubscribeFromChannelGroupRequest {
	r.keepChannels = true
	return r
}

func (r UnsubscribeFromChannelGroupRequest) Execute() (*UnsubscribeFromChannelGroupResponse, *http.Response, error) {
	return r.apiService.UnsubscribeFromChannelGroupExecute(r)
}

type UnsubscribeFromChannelGroupResponse struct {
	zulip.Response
}
