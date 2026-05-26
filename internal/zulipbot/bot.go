package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

const (
	DefaultClientName = "go-campusbot"
	DefaultRCPath     = "zuliprc"

	errContentRequired = "content must not be empty"
	errContextRequired = "context must not be nil"
)

type Bot struct {
	client  zulipclient.Client
	ownUser zulip.User
}

func New(ctx context.Context, cfg RuntimeConfig) (*Bot, error) {
	if ctx == nil {
		return nil, errors.New(errContextRequired)
	}
	if cfg.RCPath == "" {
		cfg.RCPath = DefaultRCPath
	}
	if cfg.ClientName == "" {
		cfg.ClientName = DefaultClientName
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	rc, err := zulip.NewZulipRCFromFile(cfg.RCPath)
	if err != nil {
		return nil, fmt.Errorf("load Zulip config %q: %w", cfg.RCPath, err)
	}

	apiClient, err := zulipclient.NewClient(
		rc,
		zulipclient.WithClientName(cfg.ClientName),
		zulipclient.WithLogger(cfg.Logger),
	)
	if err != nil {
		return nil, fmt.Errorf("create Zulip client: %w", err)
	}

	ownUserResp, _, err := apiClient.GetOwnUser(ctx).Execute()
	if err != nil {
		return nil, fmt.Errorf("get own Zulip user: %w", err)
	}
	if ownUserResp == nil {
		return nil, errors.New("get own Zulip user: empty response")
	}

	return &Bot{
		client:  apiClient,
		ownUser: ownUserResp.User,
	}, nil
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

// BotIdentity holds the resolved identity information for the bot account.
type BotIdentity struct {
	// IsBot is true if the authenticated account is a Zulip bot (not a regular user).
	IsBot bool
	// OwnerID is the Zulip user ID of the bot's owner, or 0 if the bot has no owner.
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

// SendReply dispatches a reply to the given target, satisfying the Messenger interface.
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

// Check implements command.Authorizer.
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

// RoleFor implements command.RoleProvider.
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
