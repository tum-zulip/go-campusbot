package zulipbot_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// --- fakeGroupSubscriber for loop tests ---

type fakeLoopGroupSubscriber struct {
	mu           sync.Mutex
	subscribed   []subscribedCall
	unsubscribed []subscribedCall
	subErr       error
	unsubErr     error
}

type subscribedCall struct {
	userID         int64
	channelGroupID int64
}

func (s *fakeLoopGroupSubscriber) SubscribeUser(_ context.Context, userID int64, channelGroupID int64) error {
	if s.subErr != nil {
		return s.subErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribed = append(s.subscribed, subscribedCall{userID, channelGroupID})
	return nil
}

func (s *fakeLoopGroupSubscriber) UnsubscribeUser(_ context.Context, userID int64, channelGroupID int64) error {
	if s.unsubErr != nil {
		return s.unsubErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubscribed = append(s.unsubscribed, subscribedCall{userID, channelGroupID})
	return nil
}

func mustReactionEvent(
	t *testing.T,
	eventID, messageID, userID int64,
	emojiName, reactionType, op string,
) events.ReactionEvent {
	t.Helper()
	data, err := json.Marshal(map[string]interface{}{
		"id":            eventID,
		"type":          "reaction",
		"op":            op,
		"message_id":    messageID,
		"user_id":       userID,
		"emoji_name":    emojiName,
		"emoji_code":    "",
		"reaction_type": reactionType,
	})
	if err != nil {
		t.Fatalf("marshal reaction event: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal reaction event: %v", err)
	}
	e, ok := envelope.Event.(events.ReactionEvent)
	if !ok {
		t.Fatalf("event type = %T, want events.ReactionEvent", envelope.Event)
	}
	return e
}

func seedAnnouncementState(t *testing.T, repo *storage.Repository, messageID int64) {
	t.Helper()
	msgID := messageID
	err := repo.SaveAnnouncementState(context.Background(), storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "hash",
	})
	if err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}
}

func seedEmojiMapping(t *testing.T, repo *storage.Repository, emojiName string, channelGroupID int64) {
	t.Helper()
	err := repo.UpsertEmojiGroupMapping(context.Background(), storage.EmojiGroupMapping{
		ShortName:      emojiName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}
}

func newTestLoopWithSubscriber(
	t *testing.T,
	repo *storage.Repository,
	sub zulipbot.GroupSubscriberForLoop,
	source zulipbot.Source,
) *zulipbot.Loop {
	t.Helper()
	registry := command.NewRegistry()
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}
	loop, err := zulipbot.NewLoop(zulipbot.LoopConfig{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
		GroupSubscriber:  sub,
	})
	if err != nil {
		t.Fatalf("NewLoop() failed: %v", err)
	}
	return loop
}

func TestLoopReactionAddSubscribesUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	// Seed announcement message and emoji mapping
	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	reactionEvent := mustReactionEvent(t, 10, 500, 123, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 1 || sub.subscribed[0].userID != 123 || sub.subscribed[0].channelGroupID != 42 {
		t.Errorf("expected subscribe(123, 42), got %v", sub.subscribed)
	}
}

func TestLoopReactionRemoveUnsubscribesUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	reactionEvent := mustReactionEvent(t, 10, 500, 123, "wi", "unicode_emoji", "remove")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.unsubscribed) != 1 || sub.unsubscribed[0].userID != 123 || sub.unsubscribed[0].channelGroupID != 42 {
		t.Errorf("expected unsubscribe(123, 42), got %v", sub.unsubscribed)
	}
}

func TestLoopReactionIgnoresNonAnnouncementMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	// messageID 999 is not the announcement message (which is 500)
	reactionEvent := mustReactionEvent(t, 10, 999, 123, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 0 {
		t.Errorf("expected no subscriptions for non-announcement message, got %v", sub.subscribed)
	}
}

func TestLoopReactionIgnoresBotOwnReaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	// userID 999 = ownUserID
	reactionEvent := mustReactionEvent(t, 10, 500, 999, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 0 {
		t.Errorf("expected no subscriptions for bot's own reaction, got %v", sub.subscribed)
	}
}

func TestLoopReactionDuplicateIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	// Two identical events with different event IDs
	e1 := mustReactionEvent(t, 10, 500, 123, "wi", "unicode_emoji", "add")
	e2 := mustReactionEvent(t, 11, 500, 123, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{e1, e2}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 1 {
		t.Errorf("expected exactly 1 subscription for duplicate events, got %d", len(sub.subscribed))
	}
}

func TestLoopReactionUnknownEmojiIgnored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	// No emoji mapping seeded

	sub := &fakeLoopGroupSubscriber{}
	reactionEvent := mustReactionEvent(t, 10, 500, 123, "unknown_emoji", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 0 {
		t.Errorf("expected no subscriptions for unknown emoji, got %v", sub.subscribed)
	}
}

func TestLoopReactionNilSubscriberIgnored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 500)
	seedEmojiMapping(t, repo, "wi", 42)

	reactionEvent := mustReactionEvent(t, 10, 500, 123, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	// nil GroupSubscriber
	registry := command.NewRegistry()
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}
	loop, err := zulipbot.NewLoop(zulipbot.LoopConfig{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
		// GroupSubscriber: nil (default)
	})
	if err != nil {
		t.Fatalf("NewLoop() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	// No panic — test passes
}

func TestLoopReactionNoAnnouncementMessageIDIgnored(t *testing.T) {
	// No announcement_state row at all → reaction events are ignored
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	// No seedAnnouncementState call
	seedEmojiMapping(t, repo, "wi", 42)

	sub := &fakeLoopGroupSubscriber{}
	reactionEvent := mustReactionEvent(t, 10, 500, 123, "wi", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.subscribed) != 0 {
		t.Errorf("expected no subscriptions when no announcement message_id, got %v", sub.subscribed)
	}
}

// fakeMissingGroupSubscriber simulates a channel group that no longer exists.
// It returns the same wrapped sentinel error the real channelgroup service produces.
type fakeMissingGroupSubscriber struct {
	mu       sync.Mutex
	addCalls int
	subCalls int
	groupID  int64
}

func (s *fakeMissingGroupSubscriber) SubscribeUser(_ context.Context, _ int64, channelGroupID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addCalls++
	s.groupID = channelGroupID
	return fmt.Errorf("subscribe user X to channel group %d: %w", channelGroupID, channelgroup.ErrChannelGroupNotFound)
}

func (s *fakeMissingGroupSubscriber) UnsubscribeUser(_ context.Context, _ int64, channelGroupID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subCalls++
	s.groupID = channelGroupID
	return fmt.Errorf(
		"unsubscribe user X from channel group %d: %w",
		channelGroupID,
		channelgroup.ErrChannelGroupNotFound,
	)
}

func TestLoopReactionMissingChannelGroupMarksFailedAndAdvancesQueue(t *testing.T) {
	// Reproduces the production bug: an enabled mapping points to a channel group
	// that does not exist. The bot must record the reaction as handled, advance
	// the queue, and not return an error that hot-loops the event handler.
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 118)
	seedEmojiMapping(t, repo, "pgdp", 30)

	sub := &fakeMissingGroupSubscriber{}
	reactionEvent := mustReactionEvent(t, 123, 118, 12, "pgdp", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{reactionEvent}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled (no infrastructure error expected)", err)
	}

	// Subscribe was attempted exactly once.
	sub.mu.Lock()
	addCalls := sub.addCalls
	sub.mu.Unlock()
	if addCalls != 1 {
		t.Errorf("expected exactly 1 SubscribeUser call, got %d", addCalls)
	}

	// Reaction must be recorded as processed so it never replays.
	processed, err := repo.IsReactionProcessed(ctx, 118, 12, "pgdp", "add")
	if err != nil {
		t.Fatalf("IsReactionProcessed: %v", err)
	}
	if !processed {
		t.Error("expected failed reaction to be marked processed to avoid hot-loop")
	}

	// last_event_id must have advanced to 123 — the queue moves on.
	state, ok, err := repo.EventQueueState(ctx)
	if err != nil {
		t.Fatalf("EventQueueState: %v", err)
	}
	if !ok || state.LastEventID != 123 {
		t.Errorf("expected last_event_id=123, got state=%+v ok=%v", state, ok)
	}
}

func TestLoopReactionMissingChannelGroupIsIdempotentAcrossReplays(t *testing.T) {
	// Even if Zulip somehow re-delivers the same reaction event (e.g. queue
	// recovery), SubscribeUser must only be called once for the same event.
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	seedAnnouncementState(t, repo, 118)
	seedEmojiMapping(t, repo, "pgdp", 30)

	sub := &fakeMissingGroupSubscriber{}
	e1 := mustReactionEvent(t, 123, 118, 12, "pgdp", "unicode_emoji", "add")
	e2 := mustReactionEvent(t, 124, 118, 12, "pgdp", "unicode_emoji", "add")
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{e1, e2}},
	}

	loop := newTestLoopWithSubscriber(t, repo, sub, source)
	_, err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.addCalls != 1 {
		t.Errorf("expected SubscribeUser to be called once across duplicate failed reactions, got %d", sub.addCalls)
	}
}

func TestLoopDMCommandsStillWorkWithReactionSubscriber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()

	registry := command.NewRegistry()
	if err := registry.Register(command.NewHelpHandler(registry, nil)); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	messenger := &recordingMessenger{}
	sub := &fakeLoopGroupSubscriber{}
	source := &fakeSource{
		registerStates: []zulipbot.QueueState{{QueueID: "q1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{mustMessageEvent(t, 2, 101, "help")}},
	}

	loop, err := zulipbot.NewLoop(zulipbot.LoopConfig{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        messenger,
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
		GroupSubscriber:  sub,
	})
	if err != nil {
		t.Fatalf("NewLoop() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if messenger.count != 1 {
		t.Fatalf("expected 1 message sent, got %d", messenger.count)
	}
}
