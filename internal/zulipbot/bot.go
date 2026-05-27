package zulipbot

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

const (
	DefaultClientName = "go-campusbot"

	errContentRequired = "content must not be empty"
	errContextRequired = "context must not be nil"

	closeDeregisterTimeout    = 5 * time.Second
	processedMessageRetention = 7 * 24 * time.Hour
	processedMessageMaxRows   = 100000

	secondsPerMinute = 60
	secondsPerHour   = 60 * secondsPerMinute
)

// ErrBadEventQueueID is returned when Zulip rejects the stored event queue ID.
var ErrBadEventQueueID = errors.New("bad Zulip event queue ID")

type QueueState struct {
	QueueID     string
	LastEventID int64
}

// GroupSubscriber handles subscribe/unsubscribe for reaction events.
type GroupSubscriber interface {
	SubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error
	UnsubscribeUser(ctx context.Context, userID int64, channelGroupID int64) error
	ChannelGroupName(ctx context.Context, channelGroupID int64) (string, error)
}

type RuntimeConfig struct {
	Logger *slog.Logger
	// RunContext is the context used for background goroutines (e.g. the
	// channel-group event listener). It should be tied to the application
	// lifetime, not to the startup timeout. If nil, the ctx passed to NewBot
	// is used as a fallback.
	RunContext context.Context
}

type Bot struct {
	client  zulipclient.Client
	ownUser zulip.User

	db        *sql.DB
	queries   *storagedb.Queries
	logger    *slog.Logger
	startedAt time.Time

	registry        *command.Registry
	argParser       *command.ArgParser
	groupSubscriber GroupSubscriber
	channelGroups   interface{ Close() error }

	accepting atomic.Bool
	requested atomic.Bool
	update    atomic.Bool

	closed atomic.Bool
}

// New creates a minimal Bot that wraps the Zulip client. Used by tests that
// exercise only the messaging/auth wrapper methods. Full event loop and
// dispatch require NewBot.
func New(ctx context.Context, client zulipclient.Client) (*Bot, error) {
	if ctx == nil {
		return nil, errors.New(errContextRequired)
	}
	if client == nil {
		return nil, errors.New("zulip client must not be nil")
	}

	ownUserResp, _, err := client.GetOwnUser(ctx).Execute()
	if err != nil {
		return nil, fmt.Errorf("get own Zulip user: %w", err)
	}
	if ownUserResp == nil {
		return nil, errors.New("get own Zulip user: empty response")
	}

	bot := &Bot{
		client:  client,
		ownUser: ownUserResp.User,
	}
	bot.accepting.Store(true)
	return bot, nil
}

// NewBot wires the full bot: client, storage queries, configuration service,
// announcement manager, channel-group client, command registry, and the
// long-poll loop. Replaces the former App.
func NewBot(
	ctx context.Context,
	cfg RuntimeConfig,
	client zulipclient.Client,
	db *sql.DB,
	queries *storagedb.Queries,
) (*Bot, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if db == nil {
		return nil, errors.New("storage database must not be nil")
	}
	if queries == nil {
		return nil, errors.New("storage queries must not be nil")
	}

	bot, err := New(ctx, client)
	if err != nil {
		return nil, err
	}

	bot.db = db
	bot.queries = queries
	bot.logger = cfg.Logger
	bot.startedAt = time.Now().UTC()

	bot.argParser = command.NewArgParser(bot)

	if err := channelgroup.Migrate(ctx, db); err != nil {
		return nil, err
	}
	var channelGroupOpts []channelgroup.ClientOption
	if cfg.RunContext != nil {
		channelGroupOpts = append(channelGroupOpts, channelgroup.WithRunContext(cfg.RunContext))
	}
	channelGroupClient, err := channelgroup.NewClient(ctx, bot.client, db, channelGroupOpts...)
	if err != nil {
		return nil, fmt.Errorf("initialize channel group client: %w", err)
	}
	if closer, ok := channelGroupClient.(interface{ Close() error }); ok {
		bot.channelGroups = closer
	}
	groupService := channelgroup.NewGroupService(channelGroupClient)
	bot.groupSubscriber = groupService

	bot.registry = command.NewRegistry()
	if err := bot.registry.Register(handlers.NewGroupHandler(
		channelGroupClient,
		queries,
		bot,
		cfg.Logger,
	)); err != nil {
		return nil, err
	}

	return bot, nil
}

func (bot *Bot) Client() zulipclient.Client {
	return bot.client
}

func (bot *Bot) OwnUser() zulip.User {
	return bot.ownUser
}

func (bot *Bot) OwnUserID() int64 {
	return bot.ownUser.UserID
}

func (bot *Bot) Accepting() bool {
	return bot.accepting.Load()
}

func (bot *Bot) RestartRequested() bool {
	if bot == nil {
		return false
	}
	return bot.requested.Load()
}

func (bot *Bot) UpdateRequested() bool {
	if bot == nil {
		return false
	}
	return bot.update.Load()
}

// Run consumes the Zulip event queue via realtimeevents.EventQueue, recovering
// expired queues by re-registering. Returns true if a restart was requested.
//
//nolint:funlen,gocognit // event-loop branching is clearer kept with the queue lifecycle it controls
func (bot *Bot) Run(ctx context.Context) (bool, error) {
	if bot.queries == nil {
		return false, errors.New("Bot.Run requires storage queries (use NewBot)")
	}

	bot.logger.DebugContext(ctx, "starting Zulip bot run loop")
	notify, err := bot.boolConfig(ctx, KeyRestartStartupNotification)
	if err != nil {
		return false, fmt.Errorf("load restart notification config: %w", err)
	}
	if notify {
		if notifyErr := bot.NotifyRestartComplete(ctx); notifyErr != nil {
			bot.logger.WarnContext(
				ctx,
				"restart completion notification failed",
				"error",
				notifyErr,
			)
		}
	} else if markErr := bot.MarkRestartComplete(ctx); markErr != nil {
		bot.logger.WarnContext(ctx, "failed to mark restart complete", "error", markErr)
	}

	if deleted, err := bot.cleanupProcessedMessages(
		ctx,
		processedMessageRetention,
		processedMessageMaxRows,
	); err != nil {
		bot.logger.WarnContext(ctx, "failed to clean processed message cache", "error", err)
	} else if deleted > 0 {
		bot.logger.DebugContext(ctx, "cleaned processed message cache", "deleted", deleted)
	}

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				bot.logger.DebugContext(ctx, "stopping Zulip bot run loop after context cancellation")
				return false, nil
			}
			return false, err
		}

		bot.logger.DebugContext(ctx, "ensuring Zulip event queue")
		state, err := bot.ensureQueue(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil
			}
			return false, err
		}

		restart, badQueue, err := bot.consumeQueue(ctx, state)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil
			}
			return false, err
		}
		if restart {
			bot.logger.DebugContext(ctx, "stopping Zulip bot run loop for requested restart")
			return true, nil
		}
		if !badQueue {
			bot.logger.DebugContext(ctx, "Zulip event queue consumer exited without restart or queue reset")
			return false, nil
		}
		bot.logger.WarnContext(
			ctx,
			"Zulip event queue expired or was pruned; registering a new queue; events may have been missed",
			"queue_id", state.QueueID,
			"last_event_id", state.LastEventID,
		)
		if err := bot.queries.ClearEventQueueState(ctx); err != nil {
			return false, err
		}
	}
}

//nolint:gocognit,funlen // Event queue handling is kept together to preserve the state-machine flow.
func (bot *Bot) consumeQueue(ctx context.Context, state QueueState) (bool, bool, error) {
	errs := make(chan error, 1)
	queue := realtimeevents.NewEventQueue(
		bot.client,
		realtimeevents.WithLogger(bot.logger),
		realtimeevents.WithEventQueueChannelErrorHandler(bot.logger, errs),
	)

	queueCtx, cancelQueue := context.WithCancel(ctx)
	defer cancelQueue()

	bot.logger.DebugContext(ctx, "connecting to Zulip event queue",
		"queue_id", state.QueueID,
		"last_event_id", state.LastEventID)
	eventCh, connectErr := queue.Connect(queueCtx, state.QueueID, state.LastEventID)
	if connectErr != nil {
		if isRecoverableEventQueueError(connectErr) {
			bot.logger.DebugContext(ctx, "Zulip event queue connect failed with recoverable error",
				"queue_id", state.QueueID,
				"error", connectErr)
			return false, true, nil
		}
		return false, false, fmt.Errorf("connect to Zulip event queue: %w", connectErr)
	}
	bot.logger.DebugContext(ctx, "connected to Zulip event queue",
		"queue_id", state.QueueID,
		"last_event_id", state.LastEventID)
	defer func() {
		if err := queue.Close(); err != nil {
			bot.logger.WarnContext(ctx, "failed to close Zulip event queue", "error", err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return false, false, nil
		case event, ok := <-eventCh:
			if !ok {
				return false, false, ctx.Err()
			}
			if event == nil {
				bot.logger.WarnContext(ctx, "received nil Zulip event")
				continue
			}
			bot.logger.DebugContext(
				ctx,
				"received Zulip event",
				"event_id",
				event.GetID(),
				"event_type",
				event.GetType(),
			)
			if err := bot.handleEvent(ctx, event, &state, queue); err != nil {
				bot.logger.ErrorContext(ctx, "failed to handle Zulip event",
					"event_id", event.GetID(),
					"event_type", event.GetType(),
					"error", err)
			}
			if err := bot.saveEventQueueState(ctx, state); err != nil {
				return false, false, err
			}
			if bot.requested.Load() {
				bot.logger.DebugContext(ctx, "restart requested after Zulip event handling",
					"event_id", event.GetID(),
					"event_type", event.GetType())
				return true, false, nil
			}
		case pollErr := <-errs:
			if isRecoverableEventQueueError(pollErr) {
				bot.logger.DebugContext(ctx, "Zulip event poll failed with recoverable error",
					"queue_id", state.QueueID,
					"error", pollErr)
				return false, true, nil
			}
			bot.logger.WarnContext(ctx, "Zulip event poll failed", "error", pollErr)
		}
	}
}

// Close deregisters the Zulip queue unless a restart is pending.
func (bot *Bot) Close() error {
	if bot == nil || bot.queries == nil {
		return nil
	}
	if !bot.closed.CompareAndSwap(false, true) {
		return nil
	}
	if bot.channelGroups != nil {
		if err := bot.channelGroups.Close(); err != nil {
			return err
		}
	}
	if !bot.requested.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), closeDeregisterTimeout)
		defer cancel()
		if err := bot.deregisterStoredQueue(ctx); err != nil {
			bot.logger.WarnContext(ctx, "failed to deregister Zulip event queue", "error", err)
		}
	}
	return nil
}

func (bot *Bot) dispatch(ctx context.Context, req command.Request) command.Result {
	result, _ := bot.dispatchOne(ctx, req)
	return result
}

func (bot *Bot) dispatchChain(
	ctx context.Context,
	req command.Request,
	chain command.Chain,
) command.Result {
	var contents []string
	var afterResponses []func(context.Context) error
	previousSucceeded := true

	for _, segment := range chain.Segments {
		if !shouldDispatchChained(segment.Operator, previousSucceeded) {
			bot.logger.DebugContext(ctx, "skipping command chain segment",
				"command", segment.Invocation.Name,
				"operator", segment.Operator,
				"previous_succeeded", previousSucceeded,
				"actor_user_id", req.Actor.UserID,
				"message_id", req.MessageID)
			continue
		}

		req.Invocation = segment.Invocation
		req.ParsedArgs = nil
		bot.logger.DebugContext(ctx, "dispatching command chain segment",
			"command", segment.Invocation.Name,
			"operator", segment.Operator,
			"previous_succeeded", previousSucceeded,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		result, succeeded := bot.dispatchOne(ctx, req)
		previousSucceeded = succeeded
		bot.logger.DebugContext(ctx, "finished command chain segment",
			"command", segment.Invocation.Name,
			"succeeded", succeeded,
			"has_content", result.Content != "",
			"has_after_response", result.AfterResponse != nil,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		if result.Content != "" {
			contents = append(contents, result.Content)
		}
		if result.AfterResponse != nil {
			afterResponses = append(afterResponses, result.AfterResponse)
		}
	}

	var afterResponse func(context.Context) error
	if len(afterResponses) > 0 {
		afterResponse = func(ctx context.Context) error {
			for _, afterResponse := range afterResponses {
				if err := afterResponse(ctx); err != nil {
					return err
				}
			}
			return nil
		}
	}

	return command.Result{
		Content:       strings.Join(contents, "\n\n"),
		AfterResponse: afterResponse,
	}
}

func shouldDispatchChained(operator command.ChainOperator, previousSucceeded bool) bool {
	switch operator {
	case command.ChainAlways, command.ChainThen:
		return true
	case command.ChainAnd:
		return previousSucceeded
	case command.ChainOr:
		return !previousSucceeded
	}
	return true
}

func permissionDeniedResult(err error) command.Result {
	if errors.Is(err, command.ErrPermissionUnavailable) {
		return command.Result{
			Content: "I cannot verify permissions right now, so I will not run that command.",
		}
	}
	return command.Result{Content: "permission denied"}
}

// dispatchOne resolves and executes a command request. Static commands (help,
// status, restart) are handled directly; everything else goes through the
// registry. The returned bool is the command-chain status: true means later
// && commands should run, false means later || commands should run.
//
//nolint:gocognit,funlen // dispatch is the central command boundary; splitting would obscure the command flow
func (bot *Bot) dispatchOne(ctx context.Context, req command.Request) (command.Result, bool) {
	name := req.Invocation.Name
	bot.logger.DebugContext(ctx, "dispatching command",
		"command", name,
		"arg_count", len(req.Invocation.Args),
		"actor_user_id", req.Actor.UserID,
		"message_id", req.MessageID)

	if !bot.accepting.Load() {
		bot.logger.DebugContext(ctx, "rejecting command while bot is not accepting commands",
			"command", name,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		return command.Result{
			Content: "The bot is restarting and is not accepting new commands right now.",
		}, false
	}

	switch name {
	case "help":
		return bot.handleHelp(ctx, req), true
	case "status":
		return bot.handleStatus(ctx, req), true
	case "restart":
		if err := bot.Check(ctx, req.Actor, restartMeta.Permission); err != nil {
			return permissionDeniedResult(err), false
		}
		return bot.handleRestart(ctx, req), true
	case "update":
		if err := bot.CheckBotOwner(req.Actor); err != nil {
			return permissionDeniedResult(err), false
		}
		return bot.handleUpdate(ctx, req), true
	case "config":
		if err := bot.Check(ctx, req.Actor, configMeta.Permission); err != nil {
			if errors.Is(err, command.ErrPermissionUnavailable) {
				return command.Result{
					Content: "I cannot verify permissions right now, so I will not run that command.",
				}, false
			}
			return command.Result{Content: "permission denied"}, false
		}
		return bot.handleConfig(ctx, req), true
	}

	handler, ok := bot.registry.Lookup(name)
	if !ok {
		bot.logger.DebugContext(ctx, "unknown command",
			"command", name,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		return command.Result{
			Content: fmt.Sprintf("Unknown command %q. Use `help` to see supported commands.", name),
		}, false
	}

	meta := handler.Metadata()
	if err := bot.Check(ctx, req.Actor, meta.Permission); err != nil {
		bot.logger.WarnContext(
			ctx,
			"command permission denied",
			"command",
			meta.Name,
			"actor_user_id",
			req.Actor.UserID,
			"error",
			err,
		)
		if errors.Is(err, command.ErrPermissionUnavailable) {
			return command.Result{
				Content: "I cannot verify permissions right now, so I will not run that command.",
			}, false
		}
		return command.Result{Content: "permission denied"}, false
	}

	if meta.ArgSpec != nil && bot.argParser != nil {
		requiredPermission := command.RequiredPermission(meta.ArgSpec, req.Invocation.Args)
		if err := bot.Check(ctx, req.Actor, requiredPermission); err != nil {
			bot.logger.WarnContext(
				ctx,
				"subcommand permission denied",
				"command",
				meta.Name,
				"actor_user_id",
				req.Actor.UserID,
				"message_id",
				req.MessageID,
				"error",
				err,
			)
			return permissionDeniedResult(err), false
		}

		bot.logger.DebugContext(ctx, "parsing command arguments",
			"command", meta.Name,
			"arg_count", len(req.Invocation.Args),
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		visibleArgSpec := command.FilterArgSpec(meta.ArgSpec, func(permission zulip.Role) bool {
			return bot.Check(ctx, req.Actor, permission) == nil
		})
		parsed, parseErr := bot.argParser.Parse(ctx, visibleArgSpec, req.Invocation.Args)
		if parseErr != nil {
			var userErr command.UserError
			if errors.As(parseErr, &userErr) {
				bot.logger.DebugContext(ctx, "command argument parsing returned user error",
					"command", meta.Name,
					"actor_user_id", req.Actor.UserID,
					"message_id", req.MessageID,
					"error", parseErr)
				return command.Result{Content: userErr.Message}, false
			}
			bot.logger.ErrorContext(
				ctx,
				"arg parsing failed",
				"command",
				meta.Name,
				"error",
				parseErr,
			)
			return command.Result{Content: "Command failed because of an internal error."}, false
		}
		req.ParsedArgs = parsed
	}

	bot.logger.DebugContext(ctx, "calling command handler",
		"command", meta.Name,
		"actor_user_id", req.Actor.UserID,
		"message_id", req.MessageID)
	result, err := handler.Handle(ctx, req)
	if err == nil {
		bot.logger.DebugContext(ctx, "command handler completed",
			"command", meta.Name,
			"has_content", result.Content != "",
			"has_after_response", result.AfterResponse != nil,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID)
		return result, true
	}

	var userErr command.UserError
	if errors.As(err, &userErr) {
		bot.logger.DebugContext(ctx, "command handler returned user error",
			"command", meta.Name,
			"actor_user_id", req.Actor.UserID,
			"message_id", req.MessageID,
			"error", err)
		return command.Result{Content: userErr.Message}, false
	}

	bot.logger.ErrorContext(
		ctx,
		"command handler failed",
		"command",
		meta.Name,
		"actor_user_id",
		req.Actor.UserID,
		"error",
		err,
	)
	return command.Result{Content: "Command failed because of an internal error."}, false
}

// --- Static command handlers ----------------------------------------------

//nolint:gochecknoglobals // static command metadata
var (
	helpMeta = command.Metadata{
		Name:       "help",
		Summary:    "Show commands available to you.",
		Usage:      "help [command]",
		Permission: command.PermOpen,
	}
	statusMeta = command.Metadata{
		Name:       "status",
		Summary:    "Show bot status and health information.",
		Usage:      "status",
		Permission: command.PermOpen,
	}
	restartMeta = command.Metadata{
		Name:       "restart",
		Summary:    "Gracefully restart the bot process.",
		Usage:      "restart [--new-queue]",
		Permission: zulip.RoleOwner,
		Privileged: true,
	}
	updateMeta = command.Metadata{
		Name:       "update",
		Summary:    "Fetch the latest release binary and restart the bot.",
		Usage:      "update [--new-queue]",
		Permission: zulip.RoleOwner,
		Privileged: true,
	}
)

func (bot *Bot) handleHelp(ctx context.Context, req command.Request) command.Result {
	role, err := bot.roleFor(ctx, req.Actor)
	if err != nil {
		role = zulip.RoleMember
	}

	metas := bot.visibleMetas(role)

	if len(req.Invocation.Args) > 0 {
		name := strings.ToLower(req.Invocation.Args[0])
		for _, meta := range metas {
			if meta.Name == name {
				return command.Result{Content: formatHelp([]command.Metadata{meta}, role)}
			}
		}
		return command.Result{Content: fmt.Sprintf("Unknown command %q.", name)}
	}
	return command.Result{Content: formatHelp(metas, role)}
}

func (bot *Bot) visibleMetas(role zulip.Role) []command.Metadata {
	all := []command.Metadata{helpMeta, statusMeta, restartMeta, updateMeta, configMeta}
	if bot.registry != nil {
		all = append(all, bot.registry.Metadata()...)
	}
	visible := make([]command.Metadata, 0, len(all))
	for _, meta := range all {
		if roleAllows(role, meta.Permission) {
			visible = append(visible, meta)
		}
	}
	sortMetas(visible)
	return visible
}

func (bot *Bot) handleStatus(ctx context.Context, req command.Request) command.Result {
	uptimeSec := int64(time.Since(bot.startedAt).Truncate(time.Second).Seconds())
	hours := uptimeSec / secondsPerHour
	minutes := (uptimeSec % secondsPerHour) / secondsPerMinute
	seconds := uptimeSec % secondsPerMinute

	accepting := "yes"
	if !bot.accepting.Load() {
		accepting = "no"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Bot status: **online**, uptime: %dh %dm %ds, accepting commands: %s",
		hours, minutes, seconds, accepting)

	if err := bot.Check(ctx, req.Actor, zulip.RoleAdmin); err == nil {
		bot.writeQueueStatus(ctx, &sb)
		bot.writeDBStatus(ctx, &sb)
		bot.writeRestartStatus(ctx, &sb)
	}

	return command.Result{Content: sb.String()}
}

func (bot *Bot) writeQueueStatus(ctx context.Context, sb *strings.Builder) {
	state, ok, err := bot.eventQueueState(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(sb, "\nqueue_status: error (%v)", err)
	case ok:
		fmt.Fprintf(sb, "\nqueue_id: %s, last_event_id: %d", state.QueueID, state.LastEventID)
	default:
		fmt.Fprintf(sb, "\nqueue_status: not registered")
	}
}

func (bot *Bot) writeDBStatus(ctx context.Context, sb *strings.Builder) {
	if err := bot.db.PingContext(ctx); err != nil {
		fmt.Fprintf(sb, "\ndb_reachable: no (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\ndb_reachable: yes")
}

func (bot *Bot) writeRestartStatus(ctx context.Context, sb *strings.Builder) {
	_, pending, err := bot.pendingRestartRequest(ctx)
	if err != nil {
		fmt.Fprintf(sb, "\nrestart_pending: error (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\nrestart_pending: %v", pending)
}

func (bot *Bot) handleRestart(_ context.Context, req command.Request) command.Result {
	newQueue, err := parseRestartArgs(req.Invocation.Args)
	if err != nil {
		return command.Result{Content: err.Error()}
	}

	content := "Restarting now. I will resume the current Zulip event queue after the process comes back; Zulip normally retains queued events for about 10 minutes."
	if newQueue {
		content = "Restarting now. I will clear the current Zulip event queue and register a new one before the process comes back."
	}

	return command.Result{
		Content: content,
		AfterResponse: func(ctx context.Context) error {
			if newQueue {
				if err := bot.replaceMainEventQueue(ctx); err != nil {
					return err
				}
			}
			_, _, err := bot.ScheduleRestart(ctx, req.Actor, req.MessageID, req.Target)
			return err
		},
	}
}

func (bot *Bot) handleUpdate(ctx context.Context, req command.Request) command.Result {
	repo, err := bot.stringConfig(ctx, KeyUpdateReleaseRepo)
	if err != nil {
		return configErrorResult(err, "read")
	}
	if repo == "" {
		return command.Result{Content: fmt.Sprintf(
			"Update release repository is not configured. Set `%s` to `owner/repo` first.",
			KeyUpdateReleaseRepo,
		)}
	}

	result := bot.handleRestart(ctx, req)
	if result.AfterResponse == nil {
		return result
	}
	result.Content = fmt.Sprintf(
		"Updating from `%s`, then restarting now. I will fetch the latest GitHub release binary before the process comes back.",
		repo,
	)
	afterRestart := result.AfterResponse
	result.AfterResponse = func(ctx context.Context) error {
		bot.update.Store(true)
		return afterRestart(ctx)
	}
	return result
}

func parseRestartArgs(args []string) (bool, error) {
	var newQueue bool
	for _, arg := range args {
		switch strings.ToLower(arg) {
		case "--new-queue", "--reset-queue", "--clear-queue":
			newQueue = true
		default:
			return false, command.NewUserError(fmt.Sprintf(
				"Unknown restart option %q. Usage: `restart [--new-queue]`.",
				arg,
			))
		}
	}
	return newQueue, nil
}

// --- Help formatting ------------------------------------------------------

func formatHelp(metas []command.Metadata, role zulip.Role) string {
	var builder strings.Builder
	builder.WriteString("Supported commands (send as a private message, no prefix needed):\n")
	for _, meta := range metas {
		usage := meta.Usage
		if meta.AdminUsage != "" && role <= zulip.RoleAdmin {
			usage = meta.AdminUsage
		}
		if meta.OwnerUsage != "" && role <= zulip.RoleOwner {
			usage = meta.OwnerUsage
		}
		lines := strings.Split(usage, "\n")
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			builder.WriteString("- `")
			builder.WriteString(line)
			builder.WriteString("`")
			if i == 0 && meta.Summary != "" {
				builder.WriteString(" — ")
				builder.WriteString(meta.Summary)
			}
			builder.WriteByte('\n')
		}
	}
	return strings.TrimSpace(builder.String())
}

func roleAllows(actorRole, requiredRole zulip.Role) bool {
	return requiredRole == 0 || actorRole <= requiredRole
}

func sortMetas(metas []command.Metadata) {
	for i := 1; i < len(metas); i++ {
		for j := i; j > 0 && metas[j-1].Name > metas[j].Name; j-- {
			metas[j-1], metas[j] = metas[j], metas[j-1]
		}
	}
}

// --- Messaging / client wrappers ------------------------------------------

func (bot *Bot) SendChannelMessage(
	ctx context.Context,
	channelID int64,
	topic string,
	content string,
) (int64, error) {
	if ctx == nil {
		return 0, errors.New(errContextRequired)
	}
	if channelID == 0 {
		return 0, errors.New("channel ID must not be zero")
	}
	if topic == "" {
		return 0, errors.New("topic must not be empty")
	}
	if content == "" {
		return 0, errors.New(errContentRequired)
	}

	resp, _, err := bot.client.SendMessage(ctx).
		To(zulip.ChannelAsRecipient(channelID)).
		Topic(topic).
		Content(content).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("send Zulip channel message: %w", err)
	}
	if resp == nil {
		return 0, errors.New("send Zulip channel message: empty response")
	}
	return resp.ID, nil
}

func (bot *Bot) sendDirectMessage(
	ctx context.Context,
	userIDs []int64,
	content string,
) (int64, error) {
	if ctx == nil {
		return 0, errors.New(errContextRequired)
	}
	if len(userIDs) == 0 {
		return 0, errors.New("at least one user ID is required")
	}
	if content == "" {
		return 0, errors.New(errContentRequired)
	}

	resp, _, err := bot.client.SendMessage(ctx).
		To(zulip.UsersAsRecipient(userIDs)).
		Content(content).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("send Zulip direct message: %w", err)
	}
	if resp == nil {
		return 0, errors.New("send Zulip direct message: empty response")
	}
	return resp.ID, nil
}

func (bot *Bot) sendReply(
	ctx context.Context,
	target command.ReplyTarget,
	content string,
) (int64, error) {
	if err := target.Validate(); err != nil {
		return 0, err
	}
	switch target.Kind {
	case command.ReplyKindChannel:
		return bot.SendChannelMessage(ctx, target.ChannelID, target.Topic, content)
	case command.ReplyKindDirect:
		return bot.sendDirectMessage(ctx, target.UserIDs, content)
	default:
		return 0, errors.New("unsupported reply target kind")
	}
}

func (bot *Bot) Check(ctx context.Context, actor command.Actor, minRole zulip.Role) error {
	if minRole == 0 {
		return nil
	}
	actorRole, err := bot.fetchRole(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("%w: %w", command.ErrPermissionUnavailable, err)
	}
	if actorRole <= minRole {
		return nil
	}
	return fmt.Errorf("%w", command.ErrDenied)
}

func (bot *Bot) CheckBotOwner(actor command.Actor) error {
	if bot.ownUser.BotOwnerID == nil {
		return fmt.Errorf("%w", command.ErrDenied)
	}
	if actor.UserID == *bot.ownUser.BotOwnerID {
		return nil
	}
	return fmt.Errorf("%w", command.ErrDenied)
}

func (bot *Bot) roleFor(ctx context.Context, actor command.Actor) (zulip.Role, error) {
	return bot.fetchRole(ctx, actor.UserID)
}

func (bot *Bot) fetchRole(ctx context.Context, userID int64) (zulip.Role, error) {
	resp, _, err := bot.client.GetUser(ctx, userID).Execute()
	if err != nil {
		return 0, fmt.Errorf("get Zulip user %d: %w", userID, err)
	}
	if resp == nil {
		return 0, fmt.Errorf("get Zulip user %d: empty response", userID)
	}
	return resp.User.Role, nil
}

func (bot *Bot) GetUserByID(ctx context.Context, id int64) (zulip.User, error) {
	resp, _, err := bot.client.GetUser(ctx, id).Execute()
	if err != nil {
		return zulip.User{}, fmt.Errorf("get Zulip user %d: %w", id, err)
	}
	if resp == nil {
		return zulip.User{}, fmt.Errorf("get Zulip user %d: empty response", id)
	}
	return resp.User, nil
}

func (bot *Bot) GetChannelByID(ctx context.Context, id int64) (zulip.Channel, error) {
	resp, _, err := bot.client.GetChannelByID(ctx, id).Execute()
	if err != nil {
		return zulip.Channel{}, fmt.Errorf("get Zulip channel %d: %w", id, err)
	}
	if resp == nil {
		return zulip.Channel{}, fmt.Errorf("get Zulip channel %d: empty response", id)
	}
	return resp.Channel, nil
}

func (bot *Bot) RenderMessage(ctx context.Context, content string) (string, error) {
	resp, _, err := bot.client.RenderMessage(ctx).Content(content).Execute()
	if err != nil {
		return "", fmt.Errorf("render Zulip message %q: %w", content, err)
	}
	if resp == nil {
		return "", fmt.Errorf("render Zulip message %q: empty response", content)
	}
	return resp.Rendered, nil
}

// --- Restart state --------------------------------------------------------

// ScheduleRestart records a restart request in storage and flips the bot to
// non-accepting state. Returns (id, scheduled, err) where scheduled is false
// if a previous request already locked in the restart.
func (bot *Bot) ScheduleRestart(
	ctx context.Context,
	actor command.Actor,
	messageID int64,
	target command.ReplyTarget,
) (int64, bool, error) {
	id, err := bot.createRestartRequest(ctx, actor.UserID, messageID, target)
	if err != nil {
		return 0, false, err
	}
	scheduled := bot.requested.CompareAndSwap(false, true)
	if scheduled {
		bot.accepting.Store(false)
	}
	return id, scheduled, nil
}

func (bot *Bot) MarkRestartInProgress(ctx context.Context) error {
	id, ok, err := bot.latestActiveRestartRequestID(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no active restart request")
	}
	return bot.markRestartInProgress(ctx, id)
}

func (bot *Bot) NotifyRestartComplete(ctx context.Context) error {
	request, ok, err := bot.pendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	messageID, sendErr := bot.sendReply(
		ctx,
		request.Target,
		"Restart complete. Event processing is back online.",
	)
	if sendErr != nil {
		if completeErr := bot.completeRestartRequest(ctx, request.ID, 0, sendErr.Error()); completeErr != nil {
			bot.logger.WarnContext(ctx, "failed to mark restart notification failure",
				"restart_request_id", request.ID, "error", completeErr)
		}
		return fmt.Errorf("send restart completion notification: %w", sendErr)
	}
	return bot.completeRestartRequest(ctx, request.ID, messageID, "")
}

func (bot *Bot) MarkRestartComplete(ctx context.Context) error {
	request, ok, err := bot.pendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return bot.completeRestartRequest(ctx, request.ID, 0, "")
}

// RestartPending reports whether a restart request is currently pending.
func (bot *Bot) RestartPending(ctx context.Context) (bool, error) {
	_, ok, err := bot.pendingRestartRequest(ctx)
	return ok, err
}

type restartRequest struct {
	ID     int64
	Target command.ReplyTarget
}

func (bot *Bot) eventQueueState(ctx context.Context) (QueueState, bool, error) {
	row, err := bot.queries.GetEventQueueState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueState{}, false, nil
	}
	if err != nil {
		return QueueState{}, false, fmt.Errorf("read event queue state: %w", err)
	}
	return QueueState{QueueID: row.QueueID, LastEventID: row.LastEventID}, true, nil
}

func (bot *Bot) saveEventQueueState(ctx context.Context, state QueueState) error {
	if state.QueueID == "" {
		return errors.New("queue ID must not be empty")
	}
	if err := bot.queries.SaveEventQueueState(ctx, storagedb.SaveEventQueueStateParams{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
		UpdatedAt:   formatTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("save event queue state: %w", err)
	}
	return nil
}

func (bot *Bot) cleanupProcessedMessages(
	ctx context.Context,
	retention time.Duration,
	maxRows int,
) (int64, error) {
	var deleted int64
	if retention > 0 {
		count, err := bot.queries.DeleteExpiredProcessedMessages(
			ctx,
			formatTime(time.Now().Add(-retention)),
		)
		if err != nil {
			return 0, fmt.Errorf("delete expired processed messages: %w", err)
		}
		deleted += count
	}
	if maxRows > 0 {
		count, err := bot.queries.TrimProcessedMessages(ctx, int64(maxRows))
		if err != nil {
			return 0, fmt.Errorf("trim processed message cache: %w", err)
		}
		deleted += count
	}
	return deleted, nil
}

func (bot *Bot) createRestartRequest(
	ctx context.Context,
	requestedByUserID int64,
	requestMessageID int64,
	target command.ReplyTarget,
) (int64, error) {
	if requestedByUserID <= 0 {
		return 0, errors.New("restart requester user ID must be positive")
	}
	if requestMessageID <= 0 {
		return 0, errors.New("restart request message ID must be positive")
	}
	if err := target.Validate(); err != nil {
		return 0, err
	}
	targetUsers, err := json.Marshal(target.UserIDs)
	if err != nil {
		return 0, fmt.Errorf("encode restart target user IDs: %w", err)
	}
	if err := bot.queries.CreateRestartRequest(ctx, storagedb.CreateRestartRequestParams{
		RequestedByUserID: requestedByUserID,
		RequestMessageID:  requestMessageID,
		ResponseKind:      string(target.Kind),
		ChannelID:         nullableInt64(target.ChannelID),
		Topic:             nullableString(target.Topic),
		RecipientUserIds:  string(targetUsers),
		RequestedAt:       formatTime(time.Now()),
	}); err != nil {
		return 0, fmt.Errorf("create restart request: %w", err)
	}
	id, err := bot.queries.GetRestartRequestIDByMessageID(ctx, requestMessageID)
	if err != nil {
		return 0, fmt.Errorf("read restart request ID: %w", err)
	}
	return id, nil
}

func (bot *Bot) pendingRestartRequest(ctx context.Context) (restartRequest, bool, error) {
	row, err := bot.queries.GetPendingRestartRequest(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return restartRequest{}, false, nil
	}
	if err != nil {
		return restartRequest{}, false, fmt.Errorf("read pending restart request: %w", err)
	}
	var userIDs []int64
	if err := json.Unmarshal([]byte(row.RecipientUserIds), &userIDs); err != nil {
		return restartRequest{}, false, fmt.Errorf("decode restart target user IDs: %w", err)
	}
	target := command.ReplyTarget{
		Kind:    command.ReplyKind(row.ResponseKind),
		Topic:   nullStringValue(row.Topic),
		UserIDs: userIDs,
	}
	if row.ChannelID.Valid {
		target.ChannelID = row.ChannelID.Int64
	}
	return restartRequest{ID: row.ID, Target: target}, true, nil
}

func (bot *Bot) latestActiveRestartRequestID(ctx context.Context) (int64, bool, error) {
	id, err := bot.queries.GetLatestActiveRestartRequestID(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read active restart request ID: %w", err)
	}
	return id, true, nil
}

func (bot *Bot) markRestartInProgress(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("restart request ID must be positive")
	}
	affected, err := bot.queries.MarkRestartInProgress(ctx, id)
	if err != nil {
		return fmt.Errorf("mark restart request %d in progress: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("restart request %d is not pending", id)
	}
	return nil
}

func (bot *Bot) completeRestartRequest(
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
	if err := bot.queries.CompleteRestartRequest(ctx, storagedb.CompleteRestartRequestParams{
		Status:              status,
		CompletedAt:         nullableString(formatTime(time.Now())),
		CompletionMessageID: nullableInt64(completionMessageID),
		Failure:             nullableString(failure),
		ID:                  id,
	}); err != nil {
		return fmt.Errorf("complete restart request %d: %w", id, err)
	}
	return nil
}

func (bot *Bot) messageProcessed(ctx context.Context, messageID int64) (bool, error) {
	_, err := bot.queries.GetProcessedMessage(ctx, messageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read processed message %d: %w", messageID, err)
	}
	return true, nil
}

func (bot *Bot) markMessageProcessed(ctx context.Context, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	if err := bot.queries.MarkMessageProcessed(ctx, storagedb.MarkMessageProcessedParams{
		MessageID:   messageID,
		ProcessedAt: formatTime(time.Now()),
	}); err != nil {
		return fmt.Errorf("mark processed message %d: %w", messageID, err)
	}
	return nil
}

func (bot *Bot) announcementState(ctx context.Context) (storagedb.AnnouncementState, bool, error) {
	row, err := bot.queries.GetAnnouncementState(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return storagedb.AnnouncementState{}, false, nil
	}
	if err != nil {
		return storagedb.AnnouncementState{}, false, fmt.Errorf("get announcement state: %w", err)
	}
	return row, true, nil
}

func (bot *Bot) emojiGroupMappingByEmoji(
	ctx context.Context,
	emojiName string,
) (storagedb.EmojiGroupMapping, bool, error) {
	row, err := bot.queries.GetEmojiGroupMappingByEmoji(ctx, emojiName)
	if errors.Is(err, sql.ErrNoRows) {
		return storagedb.EmojiGroupMapping{}, false, nil
	}
	if err != nil {
		return storagedb.EmojiGroupMapping{}, false, fmt.Errorf(
			"get emoji group mapping by emoji %q: %w",
			emojiName,
			err,
		)
	}
	return row, true, nil
}

// --- Event loop / queue / dispatch ----------------------------------------

func (bot *Bot) ensureQueue(ctx context.Context) (QueueState, error) {
	stored, ok, err := bot.eventQueueState(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if ok {
		state := QueueState{QueueID: stored.QueueID, LastEventID: stored.LastEventID}
		bot.logger.InfoContext(ctx, "resuming Zulip event queue",
			"queue_id", state.QueueID,
			"last_event_id", state.LastEventID)
		return state, nil
	}
	bot.logger.DebugContext(ctx, "no stored Zulip event queue found; registering a new queue")
	return bot.registerAndSaveQueue(ctx)
}

func (bot *Bot) registerAndSaveQueue(ctx context.Context) (QueueState, error) {
	state, err := bot.registerQueue(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if err := bot.saveEventQueueState(ctx, state); err != nil {
		return QueueState{}, err
	}
	bot.logger.InfoContext(
		ctx,
		"registered Zulip event queue",
		"queue_id",
		state.QueueID,
		"last_event_id",
		state.LastEventID,
	)
	return state, nil
}

func (bot *Bot) deregisterStoredQueue(ctx context.Context) error {
	stored, ok, err := bot.eventQueueState(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := bot.deleteQueue(ctx, stored.QueueID); err != nil {
		return err
	}
	return bot.queries.ClearEventQueueState(ctx)
}

func (bot *Bot) replaceMainEventQueue(ctx context.Context) error {
	stored, ok, err := bot.eventQueueState(ctx)
	if err != nil {
		return err
	}
	if ok {
		if err := bot.deleteQueue(ctx, stored.QueueID); err != nil {
			return err
		}
		if err := bot.queries.ClearEventQueueState(ctx); err != nil {
			return fmt.Errorf("clear event queue state: %w", err)
		}
	}
	_, err = bot.registerAndSaveQueue(ctx)
	return err
}

// registerQueue registers a broad Zulip event queue subscribed to all public channels.
func (bot *Bot) registerQueue(ctx context.Context) (QueueState, error) {
	resp, httpResp, err := bot.client.RegisterQueue(ctx).
		ApplyMarkdown(false).
		AllPublicChannels(true).
		EventTypes([]events.EventType{
			events.EventTypeHeartbeat,
			events.EventTypeMessage,
			events.EventTypeReaction,
		}).
		ClientCapabilities(map[string]interface{}{
			"empty_topic_name":           true,
			"notification_settings_null": true,
			"user_settings_object":       true,
		}).
		Execute()
	if err != nil {
		if state, decodeErr := queueStateFromRegisterHTTPResponse(httpResp); decodeErr == nil {
			return state, nil
		}
		return QueueState{}, fmt.Errorf("register Zulip event queue: %w", err)
	}
	if state, decodeErr := queueStateFromRegisterHTTPResponse(httpResp); decodeErr == nil {
		return state, nil
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return QueueState{}, errors.New("register Zulip event queue: empty queue ID")
	}
	return QueueState{QueueID: *resp.QueueID, LastEventID: resp.LastEventID}, nil
}

func (bot *Bot) deleteQueue(ctx context.Context, queueID string) error {
	if queueID == "" {
		return nil
	}
	_, _, err := bot.client.DeleteQueue(ctx).QueueID(queueID).Execute()
	if err != nil {
		return fmt.Errorf("delete Zulip event queue: %w", err)
	}
	return nil
}

func (bot *Bot) handleEvent(
	ctx context.Context,
	event events.Event,
	state *QueueState,
	queue realtimeevents.EventQueue,
) error {
	//nolint:exhaustive // unsupported event types intentionally fall through to the default state update
	switch event.GetType() {
	case events.EventTypeHeartbeat:
		state.LastEventID = event.GetID()
		return nil
	case events.EventTypeMessage:
		messageEvent, ok := event.(events.MessageEvent)
		if !ok {
			return fmt.Errorf("message event has unexpected Go type %T", event)
		}
		if err := bot.handleMessage(ctx, messageEvent, queue); err != nil {
			return err
		}
		state.LastEventID = event.GetID()
		return nil
	case events.EventTypeReaction:
		reactionEvent, ok := event.(events.ReactionEvent)
		if !ok {
			state.LastEventID = event.GetID()
			return fmt.Errorf("reaction event has unexpected Go type %T", event)
		}
		handleErr := bot.handleReaction(ctx, reactionEvent)
		state.LastEventID = event.GetID()
		return handleErr
	default:
		state.LastEventID = event.GetID()
		return nil
	}
}

//nolint:funlen // message handling is a single transactional flow with distinct early exits
func (bot *Bot) handleMessage(
	ctx context.Context,
	event events.MessageEvent,
	queue realtimeevents.EventQueue,
) error {
	msg := event.Message
	if msg.SenderID == bot.ownUser.UserID {
		bot.logger.DebugContext(ctx, "skipping own Zulip message",
			"message_id", msg.ID,
			"sender_id", msg.SenderID)
		return nil
	}
	if !msg.Type.IsDirectMessage() {
		bot.logger.DebugContext(ctx, "skipping non-direct Zulip message",
			"message_id", msg.ID,
			"sender_id", msg.SenderID,
			"message_type", msg.Type)
		return nil
	}

	alreadyProcessed, err := bot.messageProcessed(ctx, msg.ID)
	if err != nil {
		return err
	}
	if alreadyProcessed {
		bot.logger.DebugContext(
			ctx,
			"skipping already processed Zulip message",
			"message_id",
			msg.ID,
		)
		return nil
	}

	chain, err := command.ParseChain(msg.Content)
	if errors.Is(err, command.ErrNotCommand) {
		bot.logger.DebugContext(ctx, "skipping non-command Zulip message",
			"message_id", msg.ID,
			"sender_id", msg.SenderID)
		return nil
	}

	target, targetErr := replyTargetFromMessage(msg, bot.ownUser.UserID)
	if targetErr != nil {
		return targetErr
	}

	bot.logCommandReceived(ctx, msg, chain, err)
	notifier := bot.startTyping(ctx, target, queue)
	if notifier != nil {
		defer bot.stopTyping(notifier)
	}

	if err != nil {
		bot.logger.DebugContext(ctx, "malformed command message",
			"message_id", msg.ID,
			"sender_id", msg.SenderID,
			"error", err)
		if sendErr := bot.send(
			ctx,
			target,
			"Malformed command. Use `help` to see supported commands.",
		); sendErr != nil {
			return sendErr
		}
		return bot.markMessageProcessed(ctx, msg.ID)
	}

	result := bot.dispatchChain(ctx, commandRequestFromMessage(msg, target), chain)
	if result.Content != "" {
		bot.logger.DebugContext(ctx, "sending command response",
			"message_id", msg.ID,
			"target_kind", target.Kind)
		if err := bot.send(ctx, target, result.Content); err != nil {
			return err
		}
	} else {
		bot.logger.DebugContext(ctx, "command produced no response content",
			"message_id", msg.ID,
			"sender_id", msg.SenderID)
	}
	if err := bot.markMessageProcessed(ctx, msg.ID); err != nil {
		return err
	}
	if result.AfterResponse != nil {
		bot.logger.DebugContext(ctx, "running command after-response callback",
			"message_id", msg.ID,
			"sender_id", msg.SenderID)
		return result.AfterResponse(ctx)
	}
	return nil
}

func (bot *Bot) startTyping(
	ctx context.Context,
	target command.ReplyTarget,
	queue realtimeevents.EventQueue,
) *realtimeevents.TypingNotifier {
	if queue == nil {
		return nil
	}
	recipient, err := typingRecipient(target)
	if err != nil {
		bot.logger.WarnContext(ctx, "failed to resolve Zulip typing recipient", "error", err)
		return nil
	}
	notifier, err := queue.StartTyping(ctx, bot.client, recipient)
	if err != nil {
		bot.logger.WarnContext(ctx, "failed to start Zulip typing indicator", "error", err)
		return nil
	}
	return notifier
}

func (bot *Bot) stopTyping(notifier *realtimeevents.TypingNotifier) {
	if err := notifier.Close(); err != nil {
		bot.logger.WarnContext(context.Background(), "failed to stop Zulip typing indicator", "error", err)
	}
}

func typingRecipient(target command.ReplyTarget) (zulip.Recipient, error) {
	if err := target.Validate(); err != nil {
		return zulip.Recipient{}, err
	}
	switch target.Kind {
	case command.ReplyKindChannel:
		return zulip.ChannelAsRecipient(target.ChannelID), nil
	case command.ReplyKindDirect:
		return zulip.UsersAsRecipient(target.UserIDs), nil
	default:
		return zulip.Recipient{}, errors.New("unsupported reply target kind")
	}
}

func commandRequestFromMessage(msg zulip.Message, target command.ReplyTarget) command.Request {
	return command.Request{
		Actor: command.Actor{
			UserID:   msg.SenderID,
			Email:    msg.SenderEmail,
			FullName: msg.SenderFullName,
		},
		MessageID: msg.ID,
		Target:    target,
	}
}

func (bot *Bot) logCommandReceived(
	ctx context.Context,
	msg zulip.Message,
	chain command.Chain,
	parseErr error,
) {
	if parseErr != nil {
		return
	}
	bot.logger.InfoContext(
		ctx,
		"command received",
		"command",
		chain.Segments[0].Invocation.Name,
		"chain_length",
		len(chain.Segments),
		"actor_user_id",
		msg.SenderID,
	)
}

//nolint:funlen // reaction handling is a single transactional flow with distinct early exits
func (bot *Bot) handleReaction(ctx context.Context, event events.ReactionEvent) error {
	if bot.groupSubscriber == nil {
		bot.logger.DebugContext(ctx, "skipping reaction event without group subscriber",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName)
		return nil
	}

	announcementState, ok, err := bot.announcementState(ctx)
	if err != nil {
		return err
	}
	if !ok || !announcementState.MessageID.Valid {
		bot.logger.DebugContext(ctx, "skipping reaction event without announcement message",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName)
		return nil
	}
	if event.MessageID != announcementState.MessageID.Int64 {
		bot.logger.DebugContext(ctx, "skipping reaction event for non-announcement message",
			"message_id", event.MessageID,
			"announcement_message_id", announcementState.MessageID.Int64,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName)
		return nil
	}
	if event.UserID == bot.ownUser.UserID {
		bot.logger.DebugContext(ctx, "skipping own reaction event",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName)
		return nil
	}

	op, hasOp := event.GetOp()
	if !hasOp {
		bot.logger.DebugContext(ctx, "skipping reaction event without operation",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName)
		return nil
	}
	opStr := string(op)

	mapping, found, err := bot.emojiGroupMappingByEmoji(
		ctx,
		event.EmojiName,
	)
	if err != nil {
		return err
	}
	if !found {
		bot.logger.DebugContext(ctx, "skipping reaction event without emoji mapping",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName,
			"op", opStr)
		return nil
	}

	var opErr error
	//nolint:exhaustive // only add/remove reaction events change channel group membership
	switch op {
	case events.EventOpAdd:
		bot.logger.DebugContext(ctx, "subscribing user from reaction",
			"user_id", event.UserID,
			"channel_group_id", mapping.ChannelGroupID,
			"emoji_name", event.EmojiName,
			"message_id", event.MessageID)
		opErr = bot.groupSubscriber.SubscribeUser(ctx, event.UserID, mapping.ChannelGroupID)
	case events.EventOpRemove:
		bot.logger.DebugContext(ctx, "unsubscribing user from reaction",
			"user_id", event.UserID,
			"channel_group_id", mapping.ChannelGroupID,
			"emoji_name", event.EmojiName,
			"message_id", event.MessageID)
		opErr = bot.groupSubscriber.UnsubscribeUser(ctx, event.UserID, mapping.ChannelGroupID)
	default:
		bot.logger.DebugContext(ctx, "skipping unsupported reaction operation",
			"message_id", event.MessageID,
			"user_id", event.UserID,
			"emoji_name", event.EmojiName,
			"op", opStr)
		return nil
	}

	groupShortName, nameErr := bot.groupSubscriber.ChannelGroupName(ctx, mapping.ChannelGroupID)
	if nameErr != nil {
		groupShortName = fmt.Sprintf("channel_group_id:%d", mapping.ChannelGroupID)
		bot.logger.WarnContext(ctx, "failed to fetch channel group name",
			"channel_group_id", mapping.ChannelGroupID,
			"error", nameErr)
	}

	if opErr != nil {
		if errors.Is(opErr, channelgroup.ErrChannelGroupNotFound) {
			bot.logger.ErrorContext(
				ctx,
				"reaction group operation failed: channel group missing; recording as handled to avoid retry loop",
				"user_id",
				event.UserID,
				"group_short_name",
				groupShortName,
				"channel_group_id",
				mapping.ChannelGroupID,
				"op",
				opStr,
				"message_id",
				event.MessageID,
				"error",
				opErr,
			)
			return nil
		}
		bot.logger.ErrorContext(ctx, "reaction group operation failed",
			"user_id", event.UserID,
			"group_short_name", groupShortName,
			"channel_group_id", mapping.ChannelGroupID,
			"op", opStr,
			"message_id", event.MessageID,
			"error", opErr)
		return opErr
	}

	bot.logger.InfoContext(ctx, "reaction group operation completed",
		"user_id", event.UserID,
		"group_short_name", groupShortName,
		"op", opStr,
		"message_id", event.MessageID)

	return nil
}

func (bot *Bot) send(ctx context.Context, target command.ReplyTarget, content string) error {
	messageID, err := bot.sendReply(ctx, target, content)
	if err != nil {
		return err
	}
	bot.logger.DebugContext(
		ctx,
		"sent command response",
		"message_id",
		messageID,
		"target_kind",
		target.Kind,
	)
	return nil
}

// --- Free helpers ---------------------------------------------------------

func replyTargetFromMessage(msg zulip.Message, ownUserID int64) (command.ReplyTarget, error) {
	messageType := msg.Type
	if messageType.IsChannelMessage() || msg.ChannelID != nil {
		if msg.ChannelID == nil {
			return command.ReplyTarget{}, errors.New("channel message has no channel ID")
		}
		target := command.ReplyTarget{
			Kind:      command.ReplyKindChannel,
			ChannelID: *msg.ChannelID,
			Topic:     msg.Subject,
		}
		if err := target.Validate(); err != nil {
			return command.ReplyTarget{}, err
		}
		return target, nil
	}

	if messageType.IsDirectMessage() {
		userIDs := directReplyUserIDs(msg, ownUserID)
		target := command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: userIDs}
		if err := target.Validate(); err != nil {
			return command.ReplyTarget{}, err
		}
		return target, nil
	}
	return command.ReplyTarget{}, fmt.Errorf("unsupported Zulip message type %q", msg.Type)
}

func directReplyUserIDs(msg zulip.Message, ownUserID int64) []int64 {
	seen := make(map[int64]struct{})
	var userIDs []int64
	if msg.DisplayRecipient.UserRecipents != nil {
		for _, recipient := range *msg.DisplayRecipient.UserRecipents {
			if recipient.ID == nil {
				continue
			}
			id := *recipient.ID
			if id == ownUserID {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			userIDs = append(userIDs, id)
		}
	}
	if len(userIDs) == 0 && msg.SenderID != 0 && msg.SenderID != ownUserID {
		userIDs = append(userIDs, msg.SenderID)
	}
	return userIDs
}

func nullableInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: value != 0}
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
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

// --- Queue register-response decoding (moved from source.go) --------------

type registerQueueResponse struct {
	QueueID                          *string `json:"queue_id,omitempty"`
	LastEventID                      int64   `json:"last_event_id,omitempty"`
	MaxMessageID                     *int64  `json:"max_message_id,omitempty"`
	EventQueueLongpollTimeoutSeconds *int64  `json:"event_queue_longpoll_timeout_seconds,omitempty"`
	IdleQueueTimeoutSecs             *int64  `json:"idle_queue_timeout_secs,omitempty"`
}

func queueStateFromRegisterHTTPResponse(httpResp *http.Response) (QueueState, error) {
	if httpResp == nil || httpResp.Body == nil {
		return QueueState{}, errors.New("missing Zulip register response body")
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return QueueState{}, fmt.Errorf("zulip register returned HTTP %s", httpResp.Status)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return QueueState{}, err
	}
	httpResp.Body = io.NopCloser(bytes.NewReader(body))

	var resp registerQueueResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return QueueState{}, err
	}
	if resp.QueueID == nil || *resp.QueueID == "" {
		return QueueState{}, errors.New("empty queue ID")
	}
	return QueueState{QueueID: *resp.QueueID, LastEventID: resp.LastEventID}, nil
}

func isBadEventQueueID(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBadEventQueueID) {
		return true
	}
	var badQueue zulip.BadEventQueueIDError
	if errors.As(err, &badQueue) {
		return true
	}
	var coded zulip.CodedError
	return errors.As(err, &coded) && coded.Code == "BAD_EVENT_QUEUE_ID"
}

func isPrunedEventQueueError(err error) bool {
	if err == nil {
		return false
	}
	var coded zulip.CodedError
	if errors.As(err, &coded) {
		return coded.Code == "BAD_REQUEST" && strings.Contains(coded.Msg, "already been pruned")
	}
	var apiErr *zulip.APIError
	if errors.As(err, &apiErr) {
		body := string(apiErr.Body())
		return strings.Contains(body, `"code":"BAD_REQUEST"`) &&
			strings.Contains(body, "already been pruned")
	}
	message := err.Error()
	return strings.Contains(message, "BAD_REQUEST") &&
		strings.Contains(message, "already been pruned")
}

func isRecoverableEventQueueError(err error) bool {
	return isBadEventQueueID(err) || isPrunedEventQueueError(err)
}
