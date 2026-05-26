package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	z "github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const (
	processedMessageRetention = 7 * 24 * time.Hour
	processedMessageMaxRows   = 100000
	defaultMinBackoff         = time.Second
	defaultMaxBackoff         = 30 * time.Second
	defaultPollTimeout        = 90 * time.Second
	backoffMultiplier         = 2
)

// Messenger dispatches a reply to a target.
type Messenger interface {
	SendReply(ctx context.Context, target command.ReplyTarget, content string) (int64, error)
}

type Loop struct {
	source           Source
	repo             *storage.Repository
	router           *command.Router
	messenger        Messenger
	restartRequested func() bool
	ownUserID        int64
	logger           *slog.Logger
	minBackoff       time.Duration
	maxBackoff       time.Duration
	pollTimeout      time.Duration
}

type LoopConfig struct {
	Source           Source
	Repo             *storage.Repository
	Router           *command.Router
	Messenger        Messenger
	RestartRequested func() bool
	OwnUserID        int64
	Logger           *slog.Logger
	PollTimeout      time.Duration
}

func NewLoop(cfg LoopConfig) (*Loop, error) {
	if cfg.Source == nil {
		return nil, errors.New("event source must not be nil")
	}
	if cfg.Repo == nil {
		return nil, errors.New("storage repository must not be nil")
	}
	if cfg.Router == nil {
		return nil, errors.New("command router must not be nil")
	}
	if cfg.Messenger == nil {
		return nil, errors.New("messenger must not be nil")
	}
	if cfg.RestartRequested == nil {
		return nil, errors.New("restart requested callback must not be nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = defaultPollTimeout
	}
	return &Loop{
		source:           cfg.Source,
		repo:             cfg.Repo,
		router:           cfg.Router,
		messenger:        cfg.Messenger,
		restartRequested: cfg.RestartRequested,
		ownUserID:        cfg.OwnUserID,
		logger:           cfg.Logger,
		minBackoff:       defaultMinBackoff,
		maxBackoff:       defaultMaxBackoff,
		pollTimeout:      cfg.PollTimeout,
	}, nil
}

func (loop *Loop) PollTimeout() time.Duration {
	return loop.pollTimeout
}

//nolint:gocognit,funlen // long-poll loop with queue recovery, backoff, and restart exit
func (loop *Loop) Run(ctx context.Context) (bool, error) {
	if deleted, err := loop.repo.CleanupProcessedMessages(ctx, processedMessageRetention, processedMessageMaxRows); err != nil {
		loop.logger.WarnContext(ctx, "failed to clean processed message cache", "error", err)
	} else if deleted > 0 {
		loop.logger.DebugContext(ctx, "cleaned processed message cache", "deleted", deleted)
	}

	state, err := loop.ensureQueue(ctx)
	if err != nil {
		return false, err
	}

	backoff := loop.minBackoff
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}

		pollCtx, cancelPoll := context.WithTimeout(ctx, loop.pollTimeout)
		polled, err := loop.source.Poll(pollCtx, state)
		cancelPoll()
		if err != nil {
			if errors.Is(err, ErrBadEventQueueID) {
				loop.logger.WarnContext(
					ctx,
					"Zulip event queue expired; registering a new queue; events may have been missed",
					"queue_id",
					state.QueueID,
					"last_event_id",
					state.LastEventID,
				)
				loop.auditQueueRecovery(ctx, state)
				if err := loop.repo.ClearEventQueueState(ctx); err != nil {
					return false, err
				}
				state, err = loop.registerQueue(ctx)
				if err != nil {
					return false, err
				}
				backoff = loop.minBackoff
				continue
			}
			// Poll deadline exceeded but parent context is still alive: expected long-poll timeout, retry immediately.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				backoff = loop.minBackoff
				continue
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return false, err
			}
			loop.logger.WarnContext(ctx, "Zulip event poll failed", "error", err, "backoff", backoff)
			if sleepErr := sleep(ctx, backoff); sleepErr != nil {
				return false, sleepErr
			}
			backoff = nextBackoff(backoff, loop.maxBackoff)
			continue
		}
		backoff = loop.minBackoff

		for _, event := range polled {
			if event == nil {
				loop.logger.WarnContext(ctx, "received nil Zulip event")
				continue
			}
			if err := loop.handleEvent(ctx, event, &state); err != nil {
				loop.logger.ErrorContext(
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
			if err := loop.repo.SaveEventQueueState(ctx, storage.EventQueueState{
				QueueID:     state.QueueID,
				LastEventID: state.LastEventID,
			}); err != nil {
				return false, err
			}
			if loop.restartRequested() {
				return true, nil
			}
		}
	}
}

func (loop *Loop) DeregisterQueue(ctx context.Context) error {
	stored, ok, err := loop.repo.EventQueueState(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := loop.source.Delete(ctx, stored.QueueID); err != nil {
		return err
	}
	return loop.repo.ClearEventQueueState(ctx)
}

func (loop *Loop) ensureQueue(ctx context.Context) (QueueState, error) {
	stored, ok, err := loop.repo.EventQueueState(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if ok {
		state := QueueState{QueueID: stored.QueueID, LastEventID: stored.LastEventID}
		if err := loop.source.Check(ctx, state); err == nil {
			loop.logger.InfoContext(
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
		loop.logger.WarnContext(ctx, "stored Zulip event queue is no longer valid", "queue_id", state.QueueID)
		if err := loop.repo.ClearEventQueueState(ctx); err != nil {
			return QueueState{}, err
		}
	}
	return loop.registerQueue(ctx)
}

func (loop *Loop) registerQueue(ctx context.Context) (QueueState, error) {
	// Always register with broad options (all public channels, all event types).
	state, err := loop.source.Register(ctx)
	if err != nil {
		return QueueState{}, err
	}
	if err := loop.repo.SaveEventQueueState(ctx, storage.EventQueueState{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
	}); err != nil {
		return QueueState{}, err
	}
	loop.logger.InfoContext(
		ctx,
		"registered Zulip event queue",
		"queue_id",
		state.QueueID,
		"last_event_id",
		state.LastEventID,
	)
	return state, nil
}

func (loop *Loop) handleEvent(ctx context.Context, event events.Event, state *QueueState) error {
	//nolint:exhaustive // unsupported event types intentionally fall through to the default state update
	switch event.GetType() {
	case events.EventTypeHeartbeat:
		// Heartbeats are keepalives only; skip storage, just update state.
		state.LastEventID = event.GetID()
		return nil
	case events.EventTypeMessage:
		messageEvent, ok := event.(events.MessageEvent)
		if !ok {
			return fmt.Errorf("message event has unexpected Go type %T", event)
		}
		if err := loop.handleMessage(ctx, messageEvent); err != nil {
			return err
		}
		state.LastEventID = event.GetID()
		return nil
	default:
		state.LastEventID = event.GetID()
		return nil
	}
}

func (loop *Loop) handleMessage(ctx context.Context, event events.MessageEvent) error {
	msg := event.Message
	if msg.SenderID == loop.ownUserID {
		return nil
	}

	// Only process direct messages; stream/channel messages are ignored.
	if !msg.Type.IsDirectMessage() {
		return nil
	}

	alreadyProcessed, err := loop.repo.MessageProcessed(ctx, msg.ID)
	if err != nil {
		return err
	}
	if alreadyProcessed {
		loop.logger.DebugContext(ctx, "skipping already processed Zulip message", "message_id", msg.ID)
		return nil
	}

	invocation, err := command.Parse(msg.Content)
	if errors.Is(err, command.ErrNotCommand) {
		return nil
	}

	target, targetErr := replyTargetFromMessage(msg, loop.ownUserID)
	if targetErr != nil {
		return targetErr
	}

	if err == nil {
		loop.logger.InfoContext(ctx, "command received", "command", invocation.Name, "actor_user_id", msg.SenderID)
	}

	if err != nil {
		if sendErr := loop.send(ctx, target, "Malformed command. Use `help` to see supported commands."); sendErr != nil {
			return sendErr
		}
		return loop.repo.MarkMessageProcessed(ctx, msg.ID)
	}

	result := loop.router.Route(ctx, command.Request{
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
		if err := loop.send(ctx, target, result.Content); err != nil {
			return err
		}
	}
	if err := loop.repo.MarkMessageProcessed(ctx, msg.ID); err != nil {
		return err
	}
	if result.AfterResponse != nil {
		return result.AfterResponse(ctx)
	}
	return nil
}

func (loop *Loop) send(ctx context.Context, target command.ReplyTarget, content string) error {
	messageID, err := loop.messenger.SendReply(ctx, target, content)
	if err != nil {
		return err
	}
	loop.logger.DebugContext(ctx, "sent command response", "message_id", messageID, "target_kind", target.Kind)
	return nil
}

func replyTargetFromMessage(msg z.Message, ownUserID int64) (command.ReplyTarget, error) {
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

func directReplyUserIDs(msg z.Message, ownUserID int64) []int64 {
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

func (loop *Loop) auditQueueRecovery(ctx context.Context, state QueueState) {
	if err := loop.repo.RecordAudit(ctx, audit.Record{
		Action:   "event_queue.recover",
		Target:   state.QueueID,
		Status:   audit.StatusFailure,
		OldValue: fmt.Sprintf("last_event_id=%d", state.LastEventID),
		Error:    "BAD_EVENT_QUEUE_ID; registered a new queue, events after last_event_id may have been missed",
	}); err != nil {
		loop.logger.WarnContext(ctx, "failed to audit Zulip event queue recovery", "error", err)
	}
}
