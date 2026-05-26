package zulipbot

import (
	"context"
	"errors"
	"fmt"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
)

const (
	errContentRequired = "content must not be empty"
	errContextRequired = "context must not be nil"
)

type Bot struct {
	client  zulipclient.Client
	ownUser zulip.User
}

func New(ctx context.Context, cfg Config) (*Bot, error) {
	if ctx == nil {
		return nil, errors.New(errContextRequired)
	}

	cfg = cfg.withDefaults()

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

	return NewFromClient(ctx, apiClient)
}

func NewFromClient(ctx context.Context, apiClient zulipclient.Client) (*Bot, error) {
	if ctx == nil {
		return nil, errors.New(errContextRequired)
	}
	if apiClient == nil {
		return nil, errors.New("zulip client must not be nil")
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
