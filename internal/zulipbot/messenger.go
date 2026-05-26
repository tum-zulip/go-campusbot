package zulipbot

import (
	"context"
	"errors"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
)

type Messenger struct {
	bot *Bot
}

func NewMessenger(bot *Bot) *Messenger {
	return &Messenger{bot: bot}
}

func (messenger *Messenger) SendReply(ctx context.Context, target model.ReplyTarget, content string) (int64, error) {
	if messenger == nil || messenger.bot == nil {
		return 0, errors.New("messenger has no bot")
	}
	if err := target.Validate(); err != nil {
		return 0, err
	}
	switch target.Kind {
	case model.ReplyKindChannel:
		return messenger.bot.SendChannelMessage(ctx, target.ChannelID, target.Topic, content)
	case model.ReplyKindDirect:
		return messenger.bot.SendDirectMessage(ctx, target.UserIDs, content)
	default:
		return 0, errors.New("unsupported reply target")
	}
}
