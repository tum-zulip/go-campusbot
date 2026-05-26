package zulipbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const (
	DefaultClientName = "go-campusbot"

	errContentRequired = "content must not be empty"
	errContextRequired = "context must not be nil"

	closeDeregisterTimeout    = 5 * time.Second
	processedMessageRetention = 7 * 24 * time.Hour
	processedMessageMaxRows   = 100000
	defaultMinBackoff         = time.Second
	defaultMaxBackoff         = 30 * time.Second
	defaultPollTimeout        = 90 * time.Second
	backoffMultiplier         = 2

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
}

type RuntimeConfig struct {
	Logger      *slog.Logger
	PollTimeout time.Duration
	// RunContext is the context used for background goroutines (e.g. the
	// channel-group event listener). It should be tied to the application
	// lifetime, not to the startup timeout. If nil, the ctx passed to NewBot
	// is used as a fallback.
	RunContext context.Context
}

type Bot struct {
	client  zulipclient.Client
	ownUser zulip.User

	repo      *storage.Repository
	config    *configsvc.Service
	logger    *slog.Logger
	startedAt time.Time

	registry        *command.Registry
	argParser       *command.ArgParser
	groupSubscriber GroupSubscriber

	accepting atomic.Bool
	requested atomic.Bool

	closed      atomic.Bool
	minBackoff  time.Duration
	maxBackoff  time.Duration
	pollTimeout time.Duration
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

// NewBot wires the full bot: client, repository, configuration service,
// announcement manager, channel-group client, command registry, and the
// long-poll loop. Replaces the former App.
func NewBot(
	ctx context.Context,
	cfg RuntimeConfig,
	client zulipclient.Client,
	repo *storage.Repository,
) (*Bot, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if repo == nil {
		return nil, errors.New("storage repository must not be nil")
	}

	bot, err := New(ctx, client)
	if err != nil {
		return nil, err
	}

	bot.repo = repo
	bot.logger = cfg.Logger
	bot.startedAt = time.Now().UTC()
	bot.minBackoff = defaultMinBackoff
	bot.maxBackoff = defaultMaxBackoff
	bot.pollTimeout = cfg.PollTimeout
	if bot.pollTimeout == 0 {
		bot.pollTimeout = defaultPollTimeout
	}

	bot.config = configsvc.NewService(repo, bot)
	bot.argParser = command.NewArgParser(bot)

	if err := channelgroup.Migrate(ctx, repo.DB()); err != nil {
		return nil, err
	}
	var channelGroupOpts []channelgroup.ClientOption
	if cfg.RunContext != nil {
		channelGroupOpts = append(channelGroupOpts, channelgroup.WithRunContext(cfg.RunContext))
	}
	channelGroupClient, err := channelgroup.NewClient(ctx, bot.client, repo.DB(), channelGroupOpts...)
	if err != nil {
		return nil, fmt.Errorf("initialize channel group client: %w", err)
	}
	groupService := channelgroup.NewGroupService(channelGroupClient)
	bot.groupSubscriber = groupService

	announcementManager := announcement.NewManager(repo, bot, cfg.Logger)
	groupConfigReader := handlers.NewGroupConfigAdapter(
		func(ctx context.Context) (int64, bool, error) {
			v, err := bot.config.GetRaw(ctx, configsvc.KeyAnnouncementChannelID)
			if err != nil {
				return 0, false, err
			}
			if v.IsDefault || v.Value == "" {
				return 0, false, nil
			}
			id, err := strconv.ParseInt(v.Value, 10, 64)
			if err != nil {
				return 0, false, err
			}
			return id, true, nil
		},
		func(ctx context.Context) (string, bool, error) {
			v, err := bot.config.GetRaw(ctx, configsvc.KeyAnnouncementTopic)
			if err != nil {
				return "", false, err
			}
			if v.IsDefault || v.Value == "" {
				return "", false, nil
			}
			return v.Value, true, nil
		},
	)

	bot.registry = command.NewRegistry()
	if err := bot.registry.Register(handlers.NewConfigHandler(bot.config)); err != nil {
		return nil, err
	}
	if err := bot.registry.Register(handlers.NewGroupHandler(
		channelGroupClient,
		repo,
		announcementManager,
		groupConfigReader,
		bot,
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

// BotIdentity holds the resolved identity information for the bot account.
type BotIdentity struct {
	IsBot   bool
	OwnerID int64
}

// ResolveBotIdentity fetches the bot's own user details from Zulip to determine
// whether it is a bot account and who its owner is.
func (bot *Bot) ResolveBotIdentity(ctx context.Context) (BotIdentity, error) {
	resp, _, err := bot.client.GetUser(ctx, bot.ownUser.UserID).Execute()
	if err != nil {
		return BotIdentity{}, fmt.Errorf("get Zulip user details for bot identity: %w", err)
	}
	if resp == nil {
		return BotIdentity{}, errors.New("get Zulip user details: empty response")
	}
	identity := BotIdentity{IsBot: resp.User.IsBot}
	if resp.User.BotOwnerID != nil {
		identity.OwnerID = *resp.User.BotOwnerID
	}
	return identity, nil
}

// Run executes the long-poll event loop. Returns true if a restart was requested.
//
//nolint:gocognit,funlen // long-poll loop with queue recovery, backoff, and restart exit
func (bot *Bot) Run(ctx context.Context) (bool, error) {
	if bot.repo == nil {
		return false, errors.New("Bot.Run requires a repository (use NewBot)")
	}

	notify, err := bot.config.Bool(ctx, configsvc.KeyRestartStartupNotification)
	if err != nil {
		return false, fmt.Errorf("load restart notification config: %w", err)
	}
	if notify {
		if notifyErr := bot.NotifyRestartComplete(ctx); notifyErr != nil {
			bot.logger.WarnContext(ctx, "restart completion notification failed", "error", notifyErr)
		}
	} else if markErr := bot.MarkRestartComplete(ctx); markErr != nil {
		bot.logger.WarnContext(ctx, "failed to mark restart complete", "error", markErr)
	}

	if deleted, err := bot.repo.CleanupProcessedMessages(ctx, processedMessageRetention, processedMessageMaxRows); err != nil {
		bot.logger.WarnContext(ctx, "failed to clean processed message cache", "error", err)
	} else if deleted > 0 {
		bot.logger.DebugContext(ctx, "cleaned processed message cache", "deleted", deleted)
	}

	state, err := bot.ensureQueue(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, err
	}

	backoff := bot.minBackoff
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				return false, nil
			}
			return false, err
		}

		pollCtx, cancelPoll := context.WithTimeout(ctx, bot.pollTimeout)
		polled, err := bot.pollQueue(pollCtx, state)
		cancelPoll()
		if err != nil {
			if errors.Is(err, ErrBadEventQueueID) {
				bot.logger.WarnContext(
					ctx,
					"Zulip event queue expired; registering a new queue; events may have been missed",
					"queue_id",
					state.QueueID,
					"last_event_id",
					state.LastEventID,
				)
				if err := bot.repo.ClearEventQueueState(ctx); err != nil {
					return false, err
				}
				state, err = bot.registerAndSaveQueue(ctx)
				if err != nil {
					return false, err
				}
				backoff = bot.minBackoff
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				backoff = bot.minBackoff
				continue
			}
			if errors.Is(err, context.Canceled) {
				return false, nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return false, err
			}
			bot.logger.WarnContext(ctx, "Zulip event poll failed", "error", err, "backoff", backoff)
			if sleepErr := sleep(ctx, backoff); sleepErr != nil {
				if errors.Is(sleepErr, context.Canceled) {
					return false, nil
				}
				return false, sleepErr
			}
			backoff = nextBackoff(backoff, bot.maxBackoff)
			continue
		}
		backoff = bot.minBackoff

		for _, event := range polled {
			if event == nil {
				bot.logger.WarnContext(ctx, "received nil Zulip event")
				continue
			}
			if err := bot.handleEvent(ctx, event, &state); err != nil {
				bot.logger.ErrorContext(
					ctx,
					"failed to handle Zulip event",
					"event_id",
					event.GetID(),
					"event_type",
					event.GetType(),
					"error",
					err,
				)
			}
			if err := bot.repo.SaveEventQueueState(ctx, storage.EventQueueState{
				QueueID:     state.QueueID,
				LastEventID: state.LastEventID,
			}); err != nil {
				return false, err
			}
			if bot.requested.Load() {
				return true, nil
			}
		}
	}
}

// Close deregisters the Zulip queue (unless a restart is pending) and closes
// the repository.
func (bot *Bot) Close() error {
	if bot == nil || bot.repo == nil {
		return nil
	}
	if !bot.closed.CompareAndSwap(false, true) {
		return nil
	}
	if !bot.requested.Load() {
		ctx, cancel := context.WithTimeout(context.Background(), closeDeregisterTimeout)
		defer cancel()
		if err := bot.deregisterStoredQueue(ctx); err != nil {
			bot.logger.WarnContext(ctx, "failed to deregister Zulip event queue", "error", err)
		}
	}
	return bot.repo.Close()
}

// dispatch resolves and executes a command request. Static commands (help,
// status, restart) are handled directly; everything else goes through the
// registry.
func (bot *Bot) dispatch(ctx context.Context, req command.Request) command.Result {
	name := req.Invocation.Name

	if !bot.accepting.Load() {
		return command.Result{Content: "The bot is restarting and is not accepting new commands right now."}
	}

	switch name {
	case "help":
		return bot.handleHelp(ctx, req)
	case "status":
		return bot.handleStatus(ctx, req)
	case "restart":
		return bot.handleRestart(ctx, req)
	}

	handler, ok := bot.registry.Lookup(name)
	if !ok {
		return command.Result{
			Content: fmt.Sprintf("Unknown command %q. Use `help` to see supported commands.", name),
		}
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
			return command.Result{Content: "I cannot verify permissions right now, so I will not run that command."}
		}
		return command.Result{Content: "permission denied"}
	}

	if meta.ArgSpec != nil && bot.argParser != nil {
		parsed, parseErr := bot.argParser.Parse(ctx, meta.ArgSpec, req.Invocation.Args)
		if parseErr != nil {
			var userErr command.UserError
			if errors.As(parseErr, &userErr) {
				return command.Result{Content: userErr.Message}
			}
			bot.logger.ErrorContext(ctx, "arg parsing failed", "command", meta.Name, "error", parseErr)
			return command.Result{Content: "Command failed because of an internal error."}
		}
		req.ParsedArgs = parsed
	}

	result, err := handler.Handle(ctx, req)
	if err == nil {
		return result
	}

	var userErr command.UserError
	if errors.As(err, &userErr) {
		return command.Result{Content: userErr.Message}
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
	return command.Result{Content: "Command failed because of an internal error."}
}

// --- Static command handlers ----------------------------------------------

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
		Usage:      "restart",
		Permission: zulip.RoleOwner,
		Privileged: true,
	}
)

func (bot *Bot) handleHelp(ctx context.Context, req command.Request) command.Result {
	role, err := bot.RoleFor(ctx, req.Actor)
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
	all := []command.Metadata{helpMeta, statusMeta, restartMeta}
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
	state, ok, err := bot.repo.EventQueueState(ctx)
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
	if err := bot.repo.Ping(ctx); err != nil {
		fmt.Fprintf(sb, "\ndb_reachable: no (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\ndb_reachable: yes")
}

func (bot *Bot) writeRestartStatus(ctx context.Context, sb *strings.Builder) {
	_, pending, err := bot.repo.PendingRestartRequest(ctx)
	if err != nil {
		fmt.Fprintf(sb, "\nrestart_pending: error (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\nrestart_pending: %v", pending)
}

func (bot *Bot) handleRestart(_ context.Context, req command.Request) command.Result {
	return command.Result{
		Content: "Restarting now. I will resume the current Zulip event queue after the process comes back; Zulip normally retains queued events for about 10 minutes.",
		AfterResponse: func(ctx context.Context) error {
			_, _, err := bot.ScheduleRestart(ctx, req.Actor, req.MessageID, req.Target)
			return err
		},
	}
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

func (bot *Bot) SendChannelMessage(ctx context.Context, channelID int64, topic string, content string) (int64, error) {
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

func (bot *Bot) SendDirectMessage(ctx context.Context, userIDs []int64, content string) (int64, error) {
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

func (bot *Bot) SendReply(ctx context.Context, target command.ReplyTarget, content string) (int64, error) {
	if err := target.Validate(); err != nil {
		return 0, err
	}
	switch target.Kind {
	case command.ReplyKindChannel:
		return bot.SendChannelMessage(ctx, target.ChannelID, target.Topic, content)
	case command.ReplyKindDirect:
		return bot.SendDirectMessage(ctx, target.UserIDs, content)
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

func (bot *Bot) RoleFor(ctx context.Context, actor command.Actor) (zulip.Role, error) {
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

func (bot *Bot) EditMessage(ctx context.Context, messageID int64, content string) error {
	_, _, err := bot.client.UpdateMessage(ctx, messageID).Content(content).Execute()
	if err != nil {
		return fmt.Errorf("edit Zulip message %d: %w", messageID, err)
	}
	return nil
}

func (bot *Bot) AddReaction(ctx context.Context, messageID int64, emojiName, emojiCode, reactionType string) error {
	req := bot.client.AddReaction(ctx, messageID).EmojiName(emojiName)
	if emojiCode != "" {
		req = req.EmojiCode(emojiCode)
	}
	if reactionType != "" {
		req = req.ReactionType(reactionType)
	}
	_, _, err := req.Execute()
	if err != nil {
		return fmt.Errorf("add reaction %q to message %d: %w", emojiName, messageID, err)
	}
	return nil
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

func (bot *Bot) CreateChannel(
	ctx context.Context,
	name string,
	description string,
	subscriberIDs []int64,
) (int64, error) {
	if ctx == nil {
		return 0, errors.New(errContextRequired)
	}
	if name == "" {
		return 0, errors.New("channel name must not be empty")
	}
	if len(subscriberIDs) == 0 {
		return 0, errors.New("at least one subscriber ID is required")
	}

	resp, _, err := bot.client.CreateChannel(ctx).
		Name(name).
		Description(description).
		Subscribers(subscriberIDs).
		Execute()
	if err != nil {
		return 0, fmt.Errorf("create Zulip channel: %w", err)
	}
	if resp == nil {
		return 0, errors.New("create Zulip channel: empty response")
	}
	return resp.ID, nil
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
	id, err := bot.repo.CreateRestartRequest(ctx, storage.RestartRequest{
		RequestedByUserID: actor.UserID,
		RequestMessageID:  messageID,
		Target:            target,
	})
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
	id, ok, err := bot.repo.LatestActiveRestartRequestID(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no active restart request")
	}
	return bot.repo.MarkRestartInProgress(ctx, id)
}

func (bot *Bot) NotifyRestartComplete(ctx context.Context) error {
	request, ok, err := bot.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	messageID, sendErr := bot.SendReply(
		ctx,
		request.Target,
		"Restart complete. Event processing is back online.",
	)
	if sendErr != nil {
		if completeErr := bot.repo.CompleteRestartRequest(ctx, request.ID, 0, sendErr.Error()); completeErr != nil {
			bot.logger.WarnContext(ctx, "failed to mark restart notification failure",
				"restart_request_id", request.ID, "error", completeErr)
		}
		return fmt.Errorf("send restart completion notification: %w", sendErr)
	}
	return bot.repo.CompleteRestartRequest(ctx, request.ID, messageID, "")
}

func (bot *Bot) MarkRestartComplete(ctx context.Context) error {
	request, ok, err := bot.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return bot.repo.CompleteRestartRequest(ctx, request.ID, 0, "")
}

// RestartPending reports whether a restart request is currently pending.
func (bot *Bot) RestartPending(ctx context.Context) (bool, error) {
	_, ok, err := bot.repo.PendingRestartRequest(ctx)
	return ok, err
}

// --- Event loop / queue / dispatch ----------------------------------------

func (bot *Bot) ensureQueue(ctx context.Context) (QueueState, error) {
	stored, ok, err := bot.repo.EventQueueState(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if ok {
		state := QueueState{QueueID: stored.QueueID, LastEventID: stored.LastEventID}
		if err := bot.checkQueue(ctx, state); err == nil {
			bot.logger.InfoContext(
				ctx,
				"resuming Zulip event queue",
				"queue_id",
				state.QueueID,
				"last_event_id",
				state.LastEventID,
			)
			return state, nil
		} else if !errors.Is(err, ErrBadEventQueueID) {
			return QueueState{}, err
		}
		bot.logger.WarnContext(ctx, "stored Zulip event queue is no longer valid", "queue_id", state.QueueID)
		if err := bot.repo.ClearEventQueueState(ctx); err != nil {
			return QueueState{}, err
		}
	}
	return bot.registerAndSaveQueue(ctx)
}

func (bot *Bot) registerAndSaveQueue(ctx context.Context) (QueueState, error) {
	state, err := bot.registerQueue(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if err := bot.repo.SaveEventQueueState(ctx, storage.EventQueueState{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
	}); err != nil {
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
	stored, ok, err := bot.repo.EventQueueState(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := bot.deleteQueue(ctx, stored.QueueID); err != nil {
		return err
	}
	return bot.repo.ClearEventQueueState(ctx)
}

// registerQueue registers a broad Zulip event queue subscribed to all public channels.
func (bot *Bot) registerQueue(ctx context.Context) (QueueState, error) {
	resp, httpResp, err := bot.client.RegisterQueue(ctx).
		ApplyMarkdown(false).
		AllPublicChannels(true).
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

func (bot *Bot) checkQueue(ctx context.Context, state QueueState) error {
	_, _, err := bot.client.GetEvents(ctx).
		QueueID(state.QueueID).
		LastEventID(state.LastEventID).
		DontBlock(true).
		Execute()
	if err != nil {
		if IsBadEventQueueID(err) {
			return ErrBadEventQueueID
		}
		return fmt.Errorf("check Zulip event queue: %w", err)
	}
	return nil
}

func (bot *Bot) pollQueue(ctx context.Context, state QueueState) ([]events.Event, error) {
	resp, _, err := bot.client.GetEvents(ctx).
		QueueID(state.QueueID).
		LastEventID(state.LastEventID).
		Execute()
	if err != nil {
		if IsBadEventQueueID(err) {
			return nil, ErrBadEventQueueID
		}
		return nil, fmt.Errorf("poll Zulip event queue: %w", err)
	}
	if resp == nil {
		return nil, errors.New("poll Zulip event queue: empty response")
	}
	return resp.Events, nil
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

func (bot *Bot) handleEvent(ctx context.Context, event events.Event, state *QueueState) error {
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
		if err := bot.handleMessage(ctx, messageEvent); err != nil {
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

func (bot *Bot) handleMessage(ctx context.Context, event events.MessageEvent) error {
	msg := event.Message
	if msg.SenderID == bot.ownUser.UserID {
		return nil
	}
	if !msg.Type.IsDirectMessage() {
		return nil
	}

	alreadyProcessed, err := bot.repo.MessageProcessed(ctx, msg.ID)
	if err != nil {
		return err
	}
	if alreadyProcessed {
		bot.logger.DebugContext(ctx, "skipping already processed Zulip message", "message_id", msg.ID)
		return nil
	}

	invocation, err := command.Parse(msg.Content)
	if errors.Is(err, command.ErrNotCommand) {
		return nil
	}

	target, targetErr := replyTargetFromMessage(msg, bot.ownUser.UserID)
	if targetErr != nil {
		return targetErr
	}

	if err == nil {
		bot.logger.InfoContext(ctx, "command received", "command", invocation.Name, "actor_user_id", msg.SenderID)
	}

	if err != nil {
		if sendErr := bot.send(ctx, target, "Malformed command. Use `help` to see supported commands."); sendErr != nil {
			return sendErr
		}
		return bot.repo.MarkMessageProcessed(ctx, msg.ID)
	}

	result := bot.dispatch(ctx, command.Request{
		Invocation: invocation,
		Actor: command.Actor{
			UserID:   msg.SenderID,
			Email:    msg.SenderEmail,
			FullName: msg.SenderFullName,
		},
		MessageID: msg.ID,
		Target:    target,
	})
	if result.Content != "" {
		if err := bot.send(ctx, target, result.Content); err != nil {
			return err
		}
	}
	if err := bot.repo.MarkMessageProcessed(ctx, msg.ID); err != nil {
		return err
	}
	if result.AfterResponse != nil {
		return result.AfterResponse(ctx)
	}
	return nil
}

func (bot *Bot) handleReaction(ctx context.Context, event events.ReactionEvent) error {
	if bot.groupSubscriber == nil {
		return nil
	}

	announcementState, ok, err := bot.repo.GetAnnouncementState(ctx)
	if err != nil || !ok || announcementState.MessageID == nil {
		return nil
	}
	if event.MessageID != *announcementState.MessageID {
		return nil
	}
	if event.UserID == bot.ownUser.UserID {
		return nil
	}

	op, hasOp := event.GetOp()
	if !hasOp {
		return nil
	}
	opStr := string(op)

	processed, err := bot.repo.IsReactionProcessed(ctx, event.MessageID, event.UserID, event.EmojiName, opStr)
	if err != nil {
		return err
	}
	if processed {
		return nil
	}

	mapping, found, err := bot.repo.GetEmojiGroupMappingByEmoji(ctx, event.EmojiName, string(event.ReactionType))
	if err != nil {
		return err
	}
	if !found {
		return bot.repo.MarkReactionProcessed(ctx, event.MessageID, event.UserID, event.EmojiName, opStr)
	}

	var opErr error
	switch op {
	case events.EventOpAdd:
		opErr = bot.groupSubscriber.SubscribeUser(ctx, event.UserID, mapping.ChannelGroupID)
	case events.EventOpRemove:
		opErr = bot.groupSubscriber.UnsubscribeUser(ctx, event.UserID, mapping.ChannelGroupID)
	}

	if opErr != nil {
		if errors.Is(opErr, channelgroup.ErrChannelGroupNotFound) {
			bot.logger.ErrorContext(
				ctx,
				"reaction group operation failed: channel group missing; recording as handled to avoid retry loop",
				"user_id", event.UserID,
				"group_short_name", mapping.ShortName,
				"channel_group_id", mapping.ChannelGroupID,
				"op", opStr,
				"message_id", event.MessageID,
				"error", opErr,
			)
			if markErr := bot.repo.MarkReactionProcessed(ctx, event.MessageID, event.UserID, event.EmojiName, opStr); markErr != nil {
				return markErr
			}
			return nil
		}
		bot.logger.ErrorContext(ctx, "reaction group operation failed",
			"user_id", event.UserID,
			"group_short_name", mapping.ShortName,
			"channel_group_id", mapping.ChannelGroupID,
			"op", opStr,
			"message_id", event.MessageID,
			"error", opErr)
		return opErr
	}

	bot.logger.InfoContext(ctx, "reaction group operation completed",
		"user_id", event.UserID,
		"group_short_name", mapping.ShortName,
		"op", opStr,
		"message_id", event.MessageID)

	return bot.repo.MarkReactionProcessed(ctx, event.MessageID, event.UserID, event.EmojiName, opStr)
}

func (bot *Bot) send(ctx context.Context, target command.ReplyTarget, content string) error {
	messageID, err := bot.SendReply(ctx, target, content)
	if err != nil {
		return err
	}
	bot.logger.DebugContext(ctx, "sent command response", "message_id", messageID, "target_kind", target.Kind)
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

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextBackoff(current time.Duration, maxBackoff time.Duration) time.Duration {
	next := current * backoffMultiplier
	if next > maxBackoff {
		return maxBackoff
	}
	return next
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

func IsBadEventQueueID(err error) bool {
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
