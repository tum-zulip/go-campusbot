package eventloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	z "github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/lifecycle"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const (
	processedMessageRetention = 7 * 24 * time.Hour
	processedMessageMaxRows   = 100000

	rawEventRetention = 7 * 24 * time.Hour
	rawEventMaxRows   = 100000
)

type Messenger interface {
	SendReply(ctx context.Context, target model.ReplyTarget, content string) (int64, error)
}

type RestartController interface {
	RestartRequested() bool
}

type Loop struct {
	source      Source
	repo        *storage.Repository
	router      *command.Router
	messenger   Messenger
	restart     RestartController
	ownUserID   int64
	logger      *slog.Logger
	minBackoff  time.Duration
	maxBackoff  time.Duration
	pollTimeout time.Duration
}

type Config struct {
	Source      Source
	Repo        *storage.Repository
	Router      *command.Router
	Messenger   Messenger
	Restart     RestartController
	OwnUserID   int64
	Logger      *slog.Logger
	PollTimeout time.Duration
}

func New(cfg Config) (*Loop, error) {
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
	if cfg.Restart == nil {
		return nil, errors.New("restart controller must not be nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = 90 * time.Second
	}
	return &Loop{
		source:      cfg.Source,
		repo:        cfg.Repo,
		router:      cfg.Router,
		messenger:   cfg.Messenger,
		restart:     cfg.Restart,
		ownUserID:   cfg.OwnUserID,
		logger:      cfg.Logger,
		minBackoff:  time.Second,
		maxBackoff:  30 * time.Second,
		pollTimeout: cfg.PollTimeout,
	}, nil
}

func (loop *Loop) Run(ctx context.Context) error {
	if deleted, err := loop.repo.CleanupProcessedMessages(ctx, processedMessageRetention, processedMessageMaxRows); err != nil {
		loop.logger.WarnContext(ctx, "failed to clean processed message cache", "error", err)
	} else if deleted > 0 {
		loop.logger.DebugContext(ctx, "cleaned processed message cache", "deleted", deleted)
	}

	if deleted, err := loop.repo.CleanupRawEvents(ctx, rawEventRetention, rawEventMaxRows); err != nil {
		loop.logger.WarnContext(ctx, "failed to clean raw events", "error", err)
	} else if deleted > 0 {
		loop.logger.DebugContext(ctx, "cleaned raw events", "deleted", deleted)
	}

	state, err := loop.ensureQueue(ctx)
	if err != nil {
		return err
	}

	backoff := loop.minBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
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
					return err
				}
				state, err = loop.registerQueue(ctx)
				if err != nil {
					return err
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
				return err
			}
			loop.logger.WarnContext(ctx, "Zulip event poll failed", "error", err, "backoff", backoff)
			if sleepErr := sleep(ctx, backoff); sleepErr != nil {
				return sleepErr
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
				loop.logger.ErrorContext(ctx, "failed to handle Zulip event", "event_id", event.GetID(), "event_type", event.GetType(), "error", err)
			}
			if err := loop.repo.SaveEventQueueState(ctx, storage.EventQueueState{
				QueueID:     state.QueueID,
				LastEventID: state.LastEventID,
			}); err != nil {
				return err
			}
			if loop.restart.RestartRequested() {
				return lifecycle.ErrRestartRequested
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
			loop.logger.InfoContext(ctx, "resuming Zulip event queue", "queue_id", state.QueueID, "last_event_id", state.LastEventID)
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
	state, err := loop.source.Register(ctx, RegisterOptions{})
	if err != nil {
		return QueueState{}, err
	}
	if err := loop.repo.SaveEventQueueState(ctx, storage.EventQueueState{
		QueueID:     state.QueueID,
		LastEventID: state.LastEventID,
	}); err != nil {
		return QueueState{}, err
	}
	loop.logger.InfoContext(ctx, "registered Zulip event queue", "queue_id", state.QueueID, "last_event_id", state.LastEventID)
	return state, nil
}

func (loop *Loop) handleEvent(ctx context.Context, event events.Event, state *QueueState) error {
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
		loop.storeRawEventAndEnqueue(ctx, event, state.QueueID)
		return nil
	default:
		state.LastEventID = event.GetID()
		loop.storeRawEventAndEnqueue(ctx, event, state.QueueID)
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

	invocation, err := command.Parser{}.Parse(msg.Content)
	if errors.Is(err, command.ErrNotCommand) {
		return nil
	}

	target, targetErr := ReplyTargetFromMessage(msg, loop.ownUserID)
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
		Actor: model.Actor{
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

func (loop *Loop) send(ctx context.Context, target model.ReplyTarget, content string) error {
	messageID, err := loop.messenger.SendReply(ctx, target, content)
	if err != nil {
		return err
	}
	loop.logger.DebugContext(ctx, "sent command response", "message_id", messageID, "target_kind", target.Kind)
	return nil
}

// storeRawEventAndEnqueue stores a raw event and derives channel lifecycle queue entries,
// both in one transaction.
// Failure is logged but does NOT prevent command execution or event state advancement.
func (loop *Loop) storeRawEventAndEnqueue(ctx context.Context, event events.Event, queueID string) {
	rawJSON := marshalEventJSON(event)
	lifecycleItems := classifyEvent(event)

	if err := loop.repo.StoreRawEventAndEnqueueLifecycle(ctx, storage.RawEvent{
		QueueID:    queueID,
		EventID:    event.GetID(),
		EventType:  string(event.GetType()),
		ReceivedAt: time.Now().UTC(),
		RawJSON:    rawJSON,
	}, lifecycleItems); err != nil {
		loop.logger.WarnContext(ctx, "failed to store raw event or enqueue lifecycle items",
			"event_id", event.GetID(),
			"event_type", event.GetType(),
			"lifecycle_item_count", len(lifecycleItems),
			"error", err,
		)
	}
}

func marshalEventJSON(event events.Event) []byte {
	if errEvent, ok := event.(*events.EventUnmarshalingError); ok {
		return errEvent.Data
	}
	data, err := json.Marshal(event)
	if err != nil {
		return []byte(fmt.Sprintf(`{"type":%q,"id":%d}`, event.GetType(), event.GetID()))
	}
	return data
}

func ReplyTargetFromMessage(msg z.Message, ownUserID int64) (model.ReplyTarget, error) {
	messageType := msg.Type
	if messageType.IsChannelMessage() || msg.ChannelID != nil {
		if msg.ChannelID == nil {
			return model.ReplyTarget{}, errors.New("channel message has no channel ID")
		}
		target := model.ReplyTarget{
			Kind:      model.ReplyKindChannel,
			ChannelID: *msg.ChannelID,
			Topic:     msg.Subject,
		}
		if err := target.Validate(); err != nil {
			return model.ReplyTarget{}, err
		}
		return target, nil
	}

	if messageType.IsDirectMessage() {
		userIDs := directReplyUserIDs(msg, ownUserID)
		target := model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: userIDs}
		if err := target.Validate(); err != nil {
			return model.ReplyTarget{}, err
		}
		return target, nil
	}
	return model.ReplyTarget{}, fmt.Errorf("unsupported Zulip message type %q", msg.Type)
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

func nextBackoff(current time.Duration, max time.Duration) time.Duration {
	next := current * 2
	if next > max {
		return max
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
