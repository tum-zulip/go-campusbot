package eventloop

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	z "github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestLoopProcessesMessageCommandsAndPersistsEventState(t *testing.T) {
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

	message := mustMessageEvent(t, 2, 101, "help")
	source := &fakeSource{
		registerStates: []QueueState{{QueueID: "queue-1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{message}},
	}
	messenger := &recordingMessenger{}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        messenger,
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if !strings.Contains(messenger.lastContent, "Supported commands") {
		t.Fatalf("response content = %q", messenger.lastContent)
	}
	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.QueueID != "queue-1" || state.LastEventID != 2 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
	processed, err := repo.MessageProcessed(context.Background(), 101)
	if err != nil {
		t.Fatalf("MessageProcessed() failed: %v", err)
	}
	if !processed {
		t.Fatal("command message should be marked processed")
	}
}

func TestLoopRecoversFromBadStoredQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	repo := openEventLoopTestRepository(t)
	defer repo.Close()
	if err := repo.SaveEventQueueState(ctx, storage.EventQueueState{QueueID: "old", LastEventID: 10}); err != nil {
		t.Fatalf("SaveEventQueueState() failed: %v", err)
	}

	registry := command.NewRegistry()
	if err := registry.Register(command.NewHelpHandler(registry, nil)); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	source := &fakeSource{
		checkErr:       ErrBadEventQueueID,
		registerStates: []QueueState{{QueueID: "new", LastEventID: 20}},
	}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.QueueID != "new" || state.LastEventID != 20 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
}

func TestLoopSkipsDuplicateMessageEvents(t *testing.T) {
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

	source := &fakeSource{
		registerStates: []QueueState{{QueueID: "queue-1", LastEventID: 1}},
		pollBatches: [][]events.Event{{
			mustMessageEvent(t, 2, 101, "help"),
			mustMessageEvent(t, 3, 101, "help"),
		}},
	}
	messenger := &recordingMessenger{}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        messenger,
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if messenger.count != 1 {
		t.Fatalf("sent response count = %d, want 1", messenger.count)
	}
	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.LastEventID != 3 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
	count, err := repo.ProcessedMessageCount(context.Background())
	if err != nil {
		t.Fatalf("ProcessedMessageCount() failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("processed message count = %d, want 1", count)
	}
}

func TestLoopResumesStoredQueueWithoutRegistering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openEventLoopTestRepository(t)
	defer repo.Close()
	if err := repo.SaveEventQueueState(ctx, storage.EventQueueState{QueueID: "saved", LastEventID: 77}); err != nil {
		t.Fatalf("SaveEventQueueState() failed: %v", err)
	}
	registry := command.NewRegistry()
	if err := registry.Register(command.NewHelpHandler(registry, nil)); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}
	source := &fakeSource{}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if source.registerCalls != 0 {
		t.Fatalf("register calls = %d, want 0", source.registerCalls)
	}
	if len(source.pollStates) != 1 || source.pollStates[0].QueueID != "saved" ||
		source.pollStates[0].LastEventID != 77 {
		t.Fatalf("poll states = %#v", source.pollStates)
	}
}

func TestLoopRecoversFromBadQueueDuringPollAndAudits(t *testing.T) {
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
	source := &fakeSource{
		registerStates: []QueueState{
			{QueueID: "old", LastEventID: 10},
			{QueueID: "new", LastEventID: 20},
		},
		pollErrs: []error{ErrBadEventQueueID},
	}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.QueueID != "new" || state.LastEventID != 20 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
	records, err := repo.AuditRecords(context.Background())
	if err != nil {
		t.Fatalf("AuditRecords() failed: %v", err)
	}
	if len(records) != 1 || records[0].Action != "event_queue.recover" || records[0].Target != "old" {
		t.Fatalf("audit records = %#v", records)
	}
}

func TestLoopHeartbeatUpdatesOnlyEventState(t *testing.T) {
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
	source := &fakeSource{
		registerStates: []QueueState{{QueueID: "queue-1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{mustHeartbeatEvent(t, 2)}},
	}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        messenger,
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if messenger.count != 0 {
		t.Fatalf("sent response count = %d, want 0", messenger.count)
	}
	count, err := repo.ProcessedMessageCount(context.Background())
	if err != nil {
		t.Fatalf("ProcessedMessageCount() failed: %v", err)
	}
	if count != 0 {
		t.Fatalf("processed message count = %d, want 0", count)
	}
	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.LastEventID != 2 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
}

func TestLoopHandlesMalformedMessageEventWithoutAdvancing(t *testing.T) {
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
	source := &fakeSource{
		registerStates: []QueueState{{QueueID: "queue-1", LastEventID: 1}},
		pollBatches:    [][]events.Event{{fakeMalformedEvent{id: 2, eventType: events.EventTypeMessage}}},
	}
	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, err = loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	state, ok, err := repo.EventQueueState(context.Background())
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.LastEventID != 1 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}
}

type fakeSource struct {
	registerStates []QueueState
	registerCalls  int
	checkErr       error
	pollBatches    [][]events.Event
	pollErrs       []error
	pollStates     []QueueState
}

func (source *fakeSource) Register(ctx context.Context, opts RegisterOptions) (QueueState, error) {
	source.registerCalls++
	if len(source.registerStates) == 0 {
		return QueueState{QueueID: "default", LastEventID: 0}, nil
	}
	state := source.registerStates[0]
	source.registerStates = source.registerStates[1:]
	return state, nil
}

func (source *fakeSource) Check(ctx context.Context, state QueueState) error {
	return source.checkErr
}

func (source *fakeSource) Poll(ctx context.Context, state QueueState) ([]events.Event, error) {
	source.pollStates = append(source.pollStates, state)
	if len(source.pollErrs) > 0 {
		err := source.pollErrs[0]
		source.pollErrs = source.pollErrs[1:]
		return nil, err
	}
	if len(source.pollBatches) > 0 {
		batch := source.pollBatches[0]
		source.pollBatches = source.pollBatches[1:]
		return batch, nil
	}
	return nil, context.Canceled
}

func (source *fakeSource) Delete(ctx context.Context, queueID string) error {
	return nil
}

type recordingMessenger struct {
	lastContent string
	count       int
}

func (messenger *recordingMessenger) SendReply(
	_ context.Context,
	_ model.ReplyTarget,
	content string,
) (int64, error) {
	messenger.lastContent = content
	messenger.count++
	return 500, nil
}

func mustMessageEvent(t *testing.T, eventID int64, messageID int64, content string) events.MessageEvent {
	t.Helper()

	recipients := []z.UserRecipent{
		{ID: ptrInt64(123)},
		{ID: ptrInt64(999)},
	}
	envelopeJSON, err := json.Marshal(map[string]interface{}{
		"id":   eventID,
		"type": "message",
		"message": map[string]interface{}{
			"id":                messageID,
			"content":           content,
			"sender_id":         123,
			"sender_email":      "user@example.com",
			"sender_full_name":  "Test User",
			"type":              "private",
			"display_recipient": recipients,
		},
	})
	if err != nil {
		t.Fatalf("marshal message event: %v", err)
	}

	var envelope events.EventEnvelope
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		t.Fatalf("unmarshal message event: %v", err)
	}
	messageEvent, ok := envelope.Event.(events.MessageEvent)
	if !ok {
		t.Fatalf("event type = %T, want events.MessageEvent", envelope.Event)
	}
	return messageEvent
}

func mustHeartbeatEvent(t *testing.T, eventID int64) events.Event {
	t.Helper()

	envelopeJSON, err := json.Marshal(map[string]interface{}{
		"id":   eventID,
		"type": "heartbeat",
	})
	if err != nil {
		t.Fatalf("marshal heartbeat event: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		t.Fatalf("unmarshal heartbeat event: %v", err)
	}
	return envelope.Event
}

type fakeMalformedEvent struct {
	id        int64
	eventType events.EventType
}

func (event fakeMalformedEvent) GetType() events.EventType {
	return event.eventType
}

func (event fakeMalformedEvent) GetID() int64 {
	return event.id
}

func (event fakeMalformedEvent) GetOp() (events.EventOp, bool) {
	return "", false
}

func ptrInt64(value int64) *int64 {
	return &value
}

func openEventLoopTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}

// TestLoopPollTimeoutRetriesWithoutBackoff verifies that when the poll context
// times out (DeadlineExceeded from pollCtx) but the parent context is still
// alive, the loop retries immediately without treating it as a fatal error.
func TestLoopPollTimeoutRetriesWithoutBackoff(t *testing.T) {
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

	// The source first returns a DeadlineExceeded (simulating a poll timeout)
	// and then returns context.Canceled (causing the loop to exit).
	pollCallCount := 0
	source := &callbackSource{
		registerFn: func(ctx context.Context, opts RegisterOptions) (QueueState, error) {
			return QueueState{QueueID: "q1", LastEventID: 0}, nil
		},
		checkFn: func(ctx context.Context, state QueueState) error { return nil },
		pollFn: func(ctx context.Context, state QueueState) ([]events.Event, error) {
			pollCallCount++
			if pollCallCount == 1 {
				// First call: simulate poll timeout (DeadlineExceeded from poll context).
				return nil, context.DeadlineExceeded
			}
			// Second call: parent context canceled - loop should exit.
			return nil, context.Canceled
		},
		deleteFn: func(ctx context.Context, queueID string) error { return nil },
	}

	loop, err := New(Config{
		Source:           source,
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
		PollTimeout:      50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	_, runErr := loop.Run(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", runErr)
	}
	if pollCallCount != 2 {
		t.Fatalf("poll call count = %d, want 2 (poll timeout then context.Canceled)", pollCallCount)
	}
}

// TestLoopPollTimeoutDefaultIs90s verifies the default poll timeout is 90s.
func TestLoopPollTimeoutDefaultIs90s(t *testing.T) {
	t.Parallel()

	repo := openEventLoopTestRepository(t)
	defer repo.Close()
	registry := command.NewRegistry()
	router, err := command.NewRouter(command.RouterConfig{Registry: registry, Auth: allowingAuthorizer{}})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	loop, err := New(Config{
		Source:           &fakeSource{},
		Repo:             repo,
		Router:           router,
		Messenger:        &recordingMessenger{},
		RestartRequested: func() bool { return false },
		OwnUserID:        999,
		// PollTimeout not set; should default to 90s
	})
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if loop.pollTimeout != 90*time.Second {
		t.Fatalf("pollTimeout = %v, want 90s", loop.pollTimeout)
	}
}

// callbackSource is a test Source backed by function callbacks.
type callbackSource struct {
	registerFn func(ctx context.Context, opts RegisterOptions) (QueueState, error)
	checkFn    func(ctx context.Context, state QueueState) error
	pollFn     func(ctx context.Context, state QueueState) ([]events.Event, error)
	deleteFn   func(ctx context.Context, queueID string) error
}

func (s *callbackSource) Register(ctx context.Context, opts RegisterOptions) (QueueState, error) {
	return s.registerFn(ctx, opts)
}

func (s *callbackSource) Check(ctx context.Context, state QueueState) error {
	return s.checkFn(ctx, state)
}

func (s *callbackSource) Poll(ctx context.Context, state QueueState) ([]events.Event, error) {
	return s.pollFn(ctx, state)
}

func (s *callbackSource) Delete(ctx context.Context, queueID string) error {
	return s.deleteFn(ctx, queueID)
}

// allowingAuthorizer satisfies command.Authorizer and permits all actions.
type allowingAuthorizer struct{}

func (allowingAuthorizer) Check(_ context.Context, _ model.Actor, _ z.Role) error {
	return nil
}
