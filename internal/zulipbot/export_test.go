package zulipbot

import (
	"context"
	"time"

	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

func (bot *Bot) Dispatch(ctx context.Context, req command.Request) command.Result {
	return bot.dispatch(ctx, req)
}

func (bot *Bot) DispatchChain(ctx context.Context, req command.Request, chain command.Chain) command.Result {
	return bot.dispatchChain(ctx, req, chain)
}

func (bot *Bot) HandleMessage(ctx context.Context, event events.MessageEvent) error {
	return bot.handleMessage(ctx, event, realtimeevents.NewEventQueue(bot.client))
}

func (bot *Bot) HandleReaction(ctx context.Context, event events.ReactionEvent) error {
	return bot.handleReaction(ctx, event)
}

func (bot *Bot) RegisterQueueForTest(ctx context.Context) (QueueState, error) {
	return bot.registerQueue(ctx)
}

func (bot *Bot) EventQueueStateForTest(ctx context.Context) (QueueState, bool, error) {
	return bot.eventQueueState(ctx)
}

func (bot *Bot) SaveEventQueueStateForTest(ctx context.Context, state QueueState) error {
	return bot.saveEventQueueState(ctx, state)
}

func (bot *Bot) SetGroupSubscriberForTest(subscriber GroupSubscriber) {
	bot.groupSubscriber = subscriber
}

func (bot *Bot) SetStartedAtForTest(t time.Time) {
	bot.startedAt = t
}

func (bot *Bot) SetAcceptingForTest(v bool) {
	bot.accepting.Store(v)
}
