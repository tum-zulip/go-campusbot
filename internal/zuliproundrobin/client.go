package zuliproundrobin

//go:generate go run ./generate_client.go

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/client/statistics"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

var errNoClients = errors.New("zulip round-robin client requires a base client")

const (
	permissionCheckTimeout               = 10 * time.Second
	workerSubscriptionQueueRetryInterval = 5 * time.Second
	workerSubscriptionQueueDeleteTimeout = 5 * time.Second
)

var _ zulipclient.Client = (*Client)(nil)

// Client implements zulip/client.Client by forwarding user-visible requests to
// client and background lookup requests to workers in round-robin order.
type Client struct {
	mu      sync.Mutex
	nextIdx int
	client  zulipclient.Client
	workers []zulipclient.Client
}

type PublicChannelSyncResult struct {
	CheckedChannels        int
	ExistingChannels       int
	SubscribedChannels     int
	SubscribedChannelNames []string
}

type WorkerSubscriptionSyncResult struct {
	WorkerCount                     int
	MainSubscribedChannels          int
	SubscribedWorkerChannels        int
	SubscribedChannelNames          []string
	AlreadySubscribedWorkerChannels int
	UnauthorizedChannelNames        []string
}

// NewClients builds a client from an already initialized base client followed
// by optional worker clients used for round-robin request builders.
func NewClients(clients ...zulipclient.Client) (*Client, error) {
	if len(clients) == 0 || clients[0] == nil {
		return nil, errNoClients
	}
	client := clients[0]
	workers := clients[1:]
	for i, worker := range workers {
		if worker == nil {
			return nil, fmt.Errorf("zulip round-robin worker client %d is nil", i)
		}
	}
	return &Client{
		client:  client,
		workers: append([]zulipclient.Client(nil), workers...),
	}, nil
}

// NewFromFiles loads one base Zulip client from the first path and one worker
// Zulip client per remaining path.
func NewFromFiles(paths ...string) (*Client, error) {
	if len(paths) == 0 || paths[0] == "" {
		return nil, errNoClients
	}
	basePath := paths[0]
	workerPaths := paths[1:]

	logger := slog.Default()
	base, err := newClientFromFile(basePath, clientLogger(logger, "base", "base", 0, basePath))
	if err != nil {
		return nil, err
	}

	workers := make([]zulipclient.Client, 0, len(workerPaths))
	for i, path := range workerPaths {
		worker, err := newClientFromFile(
			path,
			clientLogger(logger, "worker", fmt.Sprintf("worker-%d", i+1), i+1, path),
		)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	warnOnMismatchedPermissionLevels(logger, append([]zulipclient.Client{base}, workers...), paths)
	return NewClients(append([]zulipclient.Client{base}, workers...)...)
}

// NewWithWorkerFiles builds a client from an already initialized base client
// and worker Zulip clients loaded from zuliprc paths.
func NewWithWorkerFiles(client zulipclient.Client, workerPaths ...string) (*Client, error) {
	return NewWithWorkerFilesLogger(client, slog.Default(), workerPaths...)
}

// NewWithWorkerFilesLogger builds a client from an already initialized base client
// and worker Zulip clients loaded from zuliprc paths using logger.
func NewWithWorkerFilesLogger(
	client zulipclient.Client,
	logger *slog.Logger,
	workerPaths ...string,
) (*Client, error) {
	if client == nil {
		return nil, errNoClients
	}

	workers := make([]zulipclient.Client, 0, len(workerPaths))
	for i, path := range workerPaths {
		worker, err := newClientFromFile(
			path,
			clientLogger(logger, "worker", fmt.Sprintf("worker-%d", i+1), i+1, path),
		)
		if err != nil {
			return nil, err
		}
		workers = append(workers, worker)
	}
	warnOnMismatchedPermissionLevels(
		logger,
		append([]zulipclient.Client{client}, workers...),
		append([]string{"base client"}, workerPaths...),
	)
	return NewClients(append([]zulipclient.Client{client}, workers...)...)
}

func clientLogger(
	logger *slog.Logger,
	role string,
	id string,
	index int,
	path string,
) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With(
		"zulip_client_role", role,
		"zulip_client_id", id,
		"zulip_client_index", index,
		"zuliprc", path,
	)
}

func newClientFromFile(path string, logger *slog.Logger) (zulipclient.Client, error) {
	rc, err := zulip.NewZulipRCFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("load Zulip config %q: %w", path, err)
	}
	client, err := zulipclient.NewClient(rc, zulipclient.WithLogger(logger))
	if err != nil {
		return nil, fmt.Errorf("create Zulip client for %q: %w", path, err)
	}
	return client, nil
}

type permissionLevelCheckResult struct {
	path   string
	userID int64
	email  string
	role   zulip.Role
}

func warnOnMismatchedPermissionLevels(logger *slog.Logger, clients []zulipclient.Client, paths []string) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(context.Background(), permissionCheckTimeout)
	defer cancel()

	results := make([]permissionLevelCheckResult, 0, len(clients))
	for i, client := range clients {
		ownUser, _, err := client.GetOwnUser(ctx).Execute()
		if err != nil {
			logger.Warn(
				"failed to check Zulip round-robin client permission level",
				"zuliprc", paths[i],
				"error", err,
			)
			continue
		}
		results = append(results, permissionLevelCheckResult{
			path:   paths[i],
			userID: ownUser.UserID,
			email:  ownUser.Email,
			role:   ownUser.Role,
		})
	}

	if len(results) == 0 {
		return
	}
	reference := results[0]
	for _, result := range results[1:] {
		if result.role == reference.role {
			continue
		}
		logger.Warn(
			"Zulip round-robin clients have mismatched permission levels",
			"reference_zuliprc", reference.path,
			"reference_user_id", reference.userID,
			"reference_email", reference.email,
			"reference_permission_level", roleName(reference.role),
			"mismatched_zuliprc", result.path,
			"mismatched_user_id", result.userID,
			"mismatched_email", result.email,
			"mismatched_permission_level", roleName(result.role),
		)
	}
}

func roleName(role zulip.Role) string {
	switch role {
	case zulip.RoleOwner:
		return "owner"
	case zulip.RoleAdmin:
		return "admin"
	case zulip.RoleModerator:
		return "moderator"
	case zulip.RoleMember:
		return "member"
	case zulip.RoleGuest:
		return "guest"
	default:
		return fmt.Sprintf("unknown(%d)", role)
	}
}

func (c *Client) next() zulipclient.Client {
	if len(c.workers) == 0 {
		return c.client
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	client := c.workers[c.nextIdx]
	c.nextIdx = (c.nextIdx + 1) % len(c.workers)
	return client
}

// SyncPublicChannelSubscriptions subscribes client to all active public
// channels that it can see and is not already subscribed to.
func SyncPublicChannelSubscriptions(
	ctx context.Context,
	client zulipclient.Client,
) (PublicChannelSyncResult, error) {
	if client == nil {
		return PublicChannelSyncResult{}, errNoClients
	}

	channelsResp, _, err := client.GetChannels(ctx).
		IncludePublic(true).
		IncludeWebPublic(true).
		IncludeSubscribed(true).
		ExcludeArchived(true).
		Execute()
	if err != nil {
		return PublicChannelSyncResult{}, fmt.Errorf("get public Zulip channels: %w", err)
	}
	subscriptionsResp, _, err := client.GetSubscriptions(ctx).Execute()
	if err != nil {
		return PublicChannelSyncResult{}, fmt.Errorf("get Zulip subscriptions: %w", err)
	}

	subscribed := subscribedChannelSet(subscriptionsResp.Subscriptions)
	missing := make([]channels.SubscriptionRequest, 0)
	for _, channel := range channelsResp.Channels {
		if channel.IsArchived || channel.InviteOnly || channel.Name == "" {
			continue
		}
		if _, ok := subscribed[channelKey(channel.ChannelID, channel.Name)]; ok {
			continue
		}
		missing = append(missing, channels.SubscriptionRequest{Name: channel.Name})
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })

	result := PublicChannelSyncResult{
		CheckedChannels:    len(channelsResp.Channels),
		ExistingChannels:   len(subscriptionsResp.Subscriptions),
		SubscribedChannels: len(missing),
	}
	for _, subscription := range missing {
		result.SubscribedChannelNames = append(result.SubscribedChannelNames, subscription.Name)
	}
	if len(missing) == 0 {
		return result, nil
	}
	if _, _, err := client.Subscribe(ctx).
		Subscriptions(missing).
		SendNewSubscriptionMessages(false).
		Execute(); err != nil {
		return PublicChannelSyncResult{}, fmt.Errorf("subscribe main Zulip client to public channels: %w", err)
	}
	return result, nil
}

// StartWorkerSubscriptionSyncQueue starts a dedicated Zulip subscription event
// queue for the base client. Whenever the base client is subscribed to new
// channels, all workers are subscribed to those channels in one bulk request.
func (c *Client) StartWorkerSubscriptionSyncQueue(ctx context.Context, logger *slog.Logger) error {
	if c == nil || c.client == nil {
		return errNoClients
	}
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	if len(c.workers) == 0 {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	go c.runWorkerSubscriptionSyncQueue(ctx, logger)
	return nil
}

func (c *Client) runWorkerSubscriptionSyncQueue(ctx context.Context, logger *slog.Logger) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := c.consumeWorkerSubscriptionSyncQueue(ctx, logger); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "Zulip worker subscription sync event queue failed", "error", err)
			if !waitWorkerSubscriptionQueueRetry(ctx) {
				return
			}
		}
	}
}

//nolint:funlen // this function is long but clear and well-structured, and splitting it up would not improve readability
func (c *Client) consumeWorkerSubscriptionSyncQueue(ctx context.Context, logger *slog.Logger) error {
	resp, _, err := c.client.RegisterQueue(ctx).
		ApplyMarkdown(false).
		EventTypes([]events.EventType{events.EventTypeSubscription}).
		ClientCapabilities(map[string]interface{}{
			"archived_channels":          true,
			"notification_settings_null": true,
		}).
		Execute()
	if err != nil {
		return fmt.Errorf("register Zulip worker subscription sync event queue: %w", err)
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return errors.New("register Zulip worker subscription sync event queue: empty queue ID")
	}

	queueID := *resp.QueueID
	errs := make(chan error, 1)
	queue := realtimeevents.NewEventQueue(
		c.client,
		realtimeevents.WithLogger(logger),
		realtimeevents.WithEventQueueChannelErrorHandler(logger, errs),
	)

	queueCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	connected := false
	defer func() {
		if connected {
			if err := queue.Close(); err != nil {
				logger.WarnContext(ctx, "failed to close Zulip worker subscription sync event queue", "error", err)
			}
		}
		deleteCtx, cancelDelete := context.WithTimeout(context.Background(), workerSubscriptionQueueDeleteTimeout)
		defer cancelDelete()
		if _, _, err := c.client.DeleteQueue(deleteCtx).QueueID(queueID).Execute(); err != nil {
			logger.WarnContext(ctx, "failed to delete Zulip worker subscription sync event queue", "error", err)
		}
	}()

	eventCh, err := queue.Connect(queueCtx, queueID, resp.LastEventID)
	if err != nil {
		return fmt.Errorf("connect Zulip worker subscription sync event queue: %w", err)
	}
	connected = true

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return fmt.Errorf("poll Zulip worker subscription sync event queue: %w", err)
		case event, ok := <-eventCh:
			if !ok {
				return errors.New("zulip worker subscription sync event queue closed")
			}
			if event == nil {
				logger.WarnContext(ctx, "received nil Zulip worker subscription sync event")
				continue
			}
			c.handleWorkerSubscriptionSyncEvent(ctx, logger, event)
		}
	}
}

func (c *Client) handleWorkerSubscriptionSyncEvent(
	ctx context.Context,
	logger *slog.Logger,
	event events.Event,
) {
	if op, ok := event.GetOp(); ok && op != events.EventOpAdd {
		return
	}
	add, ok := event.(events.SubscriptionAddEvent)
	if !ok {
		return
	}
	result, err := c.SyncWorkerSubscriptionsToChannels(ctx, add.Subscriptions)
	if err != nil {
		logger.ErrorContext(ctx, "failed to sync Zulip workers after main client subscription event",
			"event_id", event.GetID(),
			"subscribed_channels", result.SubscribedChannelNames,
			"error", err,
		)
		return
	}
	if result.MainSubscribedChannels == 0 {
		return
	}
	logger.InfoContext(ctx, "synced Zulip workers after main client subscription event",
		"event_id", event.GetID(),
		"workers", result.WorkerCount,
		"main_subscribed_channels", result.MainSubscribedChannels,
		"subscribed_worker_channels", result.SubscribedWorkerChannels,
		"already_subscribed_worker_channels", result.AlreadySubscribedWorkerChannels,
	)
}

// SyncWorkerSubscriptions subscribes every worker client to all active channels
// that the base client is subscribed to, including private channels.
func (c *Client) SyncWorkerSubscriptions(ctx context.Context) (WorkerSubscriptionSyncResult, error) {
	if c == nil || c.client == nil {
		return WorkerSubscriptionSyncResult{}, errNoClients
	}

	subscriptionsResp, _, err := c.client.GetSubscriptions(ctx).Execute()
	if err != nil {
		return WorkerSubscriptionSyncResult{}, fmt.Errorf("get main Zulip client subscriptions: %w", err)
	}
	return c.SyncWorkerSubscriptionsToChannels(ctx, subscriptionsResp.Subscriptions)
}

// SyncWorkerSubscriptionsToChannels subscribes every worker client to the
// provided main-client subscriptions.
//
//nolint:funlen // this function is long but clear and well-structured, and splitting it up would not improve readability
func (c *Client) SyncWorkerSubscriptionsToChannels(
	ctx context.Context,
	mainSubscriptions []zulip.Subscription,
) (WorkerSubscriptionSyncResult, error) {
	if c == nil || c.client == nil {
		return WorkerSubscriptionSyncResult{}, errNoClients
	}
	if len(c.workers) == 0 {
		return WorkerSubscriptionSyncResult{}, nil
	}

	workerIDs, err := c.workerUserIDs(ctx)
	if err != nil {
		return WorkerSubscriptionSyncResult{}, err
	}

	seen := make(map[string]struct{}, len(mainSubscriptions))
	subscriptions := make([]channels.SubscriptionRequest, 0, len(mainSubscriptions))
	for _, subscription := range mainSubscriptions {
		if subscription.IsArchived || subscription.Name == "" {
			continue
		}
		if _, ok := seen[subscription.Name]; ok {
			continue
		}
		seen[subscription.Name] = struct{}{}
		subscriptions = append(subscriptions, channels.SubscriptionRequest{Name: subscription.Name})
	}
	sort.Slice(subscriptions, func(i, j int) bool { return subscriptions[i].Name < subscriptions[j].Name })

	result := WorkerSubscriptionSyncResult{
		WorkerCount:            len(workerIDs),
		MainSubscribedChannels: len(subscriptions),
	}
	for _, subscription := range subscriptions {
		result.SubscribedChannelNames = append(result.SubscribedChannelNames, subscription.Name)
	}
	if len(subscriptions) == 0 {
		return result, nil
	}

	resp, _, err := c.client.Subscribe(ctx).
		Subscriptions(subscriptions).
		Principals(zulip.UserIDsAsPrincipals(workerIDs...)).
		AuthorizationErrorsFatal(false).
		SendNewSubscriptionMessages(false).
		Execute()
	if err != nil {
		return WorkerSubscriptionSyncResult{}, fmt.Errorf("subscribe Zulip workers to main client channels: %w", err)
	}
	if resp != nil {
		result.SubscribedWorkerChannels = subscriptionResponseCount(resp.Subscribed)
		result.AlreadySubscribedWorkerChannels = subscriptionResponseCount(resp.AlreadySubscribed)
		result.UnauthorizedChannelNames = append([]string(nil), resp.Unauthorized...)
		sort.Strings(result.UnauthorizedChannelNames)
	}
	if len(result.UnauthorizedChannelNames) > 0 {
		return result, fmt.Errorf(
			"subscribe Zulip workers to main client channels: unauthorized channels: %s",
			strings.Join(result.UnauthorizedChannelNames, ", "),
		)
	}
	return result, nil
}

func (c *Client) workerUserIDs(ctx context.Context) ([]int64, error) {
	workerIDs := make([]int64, 0, len(c.workers))
	for i, worker := range c.workers {
		ownUser, _, err := worker.GetOwnUser(ctx).Execute()
		if err != nil {
			return nil, fmt.Errorf("get Zulip worker %d own user: %w", i+1, err)
		}
		if ownUser == nil || ownUser.UserID == 0 {
			return nil, fmt.Errorf("get Zulip worker %d own user: missing user ID", i+1)
		}
		workerIDs = append(workerIDs, ownUser.UserID)
	}
	return workerIDs, nil
}

func waitWorkerSubscriptionQueueRetry(ctx context.Context) bool {
	timer := time.NewTimer(workerSubscriptionQueueRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func subscribedChannelSet(subscriptions []zulip.Subscription) map[string]struct{} {
	subscribed := make(map[string]struct{})
	for _, subscription := range subscriptions {
		if subscription.ChannelID != 0 {
			subscribed[channelKey(subscription.ChannelID, "")] = struct{}{}
		}
		if subscription.Name != "" {
			subscribed[channelKey(0, subscription.Name)] = struct{}{}
		}
	}
	return subscribed
}

func channelKey(channelID int64, name string) string {
	if channelID != 0 {
		return strconv.FormatInt(channelID, 10)
	}
	return name
}

func subscriptionResponseCount(values map[string][]string) int {
	var count int
	for _, channels := range values {
		count += len(channels)
	}
	return count
}

func (c *Client) GetStatistics() statistics.Statistics {
	merged := statistics.Statistics{Stats: map[string]statistics.Statistic{}}
	for _, client := range append([]zulipclient.Client{c.client}, c.workers...) {
		for endpoint, stat := range client.GetStatistics().Stats {
			current := merged.Stats[endpoint]
			current.Count += stat.Count
			current.ErrCount += stat.ErrCount
			current.RetryCount += stat.RetryCount
			current.TotalDuration += stat.TotalDuration
			merged.Stats[endpoint] = current
		}
	}
	return merged
}
