package zulipbot

import (
	"context"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

func (bot *Bot) Dispatch(ctx context.Context, req command.Request) command.Result {
	return bot.dispatch(ctx, req)
}

func (bot *Bot) SetStartedAtForTest(t time.Time) {
	bot.startedAt = t
}

func (bot *Bot) SetAcceptingForTest(v bool) {
	bot.accepting.Store(v)
}
