package zulipbot_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

type recordingGroupSubscriber struct {
	subscribedUserID  int64
	subscribedGroupID int64
}

func (s *recordingGroupSubscriber) SubscribeUser(
	_ context.Context,
	userID int64,
	channelGroupID int64,
) error {
	s.subscribedUserID = userID
	s.subscribedGroupID = channelGroupID
	return nil
}

func (s *recordingGroupSubscriber) UnsubscribeUser(
	_ context.Context,
	_ int64,
	_ int64,
) error {
	return nil
}

func (s *recordingGroupSubscriber) ChannelGroupName(
	_ context.Context,
	channelGroupID int64,
) (string, error) {
	return "group-" + strconv.FormatInt(channelGroupID, 10), nil
}

func decodeReactionEvent(t *testing.T, raw string) events.ReactionEvent {
	t.Helper()

	var envelope events.EventEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode reaction event: %v", err)
	}
	event, ok := envelope.Event.(events.ReactionEvent)
	if !ok {
		t.Fatalf("decoded event type = %T, want events.ReactionEvent", envelope.Event)
	}
	return event
}

func newDispatchTestBot(t *testing.T) *zulipbot.Bot {
	t.Helper()

	client := zulipmock.NewClient()
	client.SetOwnUser(zulip.User{UserID: 100, Email: "bot@example.com", FullName: "Mock Bot", IsBot: true})
	client.AddUser(zulip.User{UserID: 100, IsBot: true})
	client.AddUser(zulip.User{UserID: 7, Role: zulip.RoleMember})
	client.AddUser(zulip.User{UserID: 9, Role: zulip.RoleOwner})

	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	db, queries := openZulipbotTestStorage(t, dbPath)

	bot, err := zulipbot.NewBot(
		context.Background(),
		zulipbot.RuntimeConfig{Logger: slog.Default()},
		client,
		db,
		queries,
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bot.SetStartedAtForTest(time.Now().Add(-2 * time.Hour))
	return bot
}

func memberRequest(name string, args ...string) command.Request {
	return command.Request{
		Invocation: command.Invocation{Name: name, Args: args},
		Actor:      command.Actor{UserID: 7},
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{7}},
	}
}

func ownerRequest(name string, args ...string) command.Request {
	return command.Request{
		Invocation: command.Invocation{Name: name, Args: args},
		Actor:      command.Actor{UserID: 9},
		MessageID:  42,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{9}},
	}
}

func TestDispatchHelpListsCommands(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), memberRequest("help"))
	if !strings.Contains(result.Content, "Supported commands") {
		t.Fatalf("help output = %q", result.Content)
	}
	if !strings.Contains(result.Content, "help") || !strings.Contains(result.Content, "status") {
		t.Fatalf("help should list help and status: %q", result.Content)
	}
	if strings.Contains(result.Content, "restart") {
		t.Fatalf("member must not see restart command: %q", result.Content)
	}
}

func TestDispatchStatusIncludesUptime(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), memberRequest("status"))
	if !strings.Contains(result.Content, "uptime: 2h") {
		t.Fatalf("status should report uptime: %q", result.Content)
	}
	if !strings.Contains(result.Content, "accepting commands: yes") {
		t.Fatalf("status should report accepting=yes: %q", result.Content)
	}
	if strings.Contains(result.Content, "queue_id") {
		t.Fatalf("member must not see admin status fields: %q", result.Content)
	}
}

func TestDispatchRestartSchedulesRestart(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	ctx := context.Background()
	req := ownerRequest("restart")

	result := bot.Dispatch(ctx, req)
	if !strings.Contains(result.Content, "Restarting now") {
		t.Fatalf("restart reply = %q", result.Content)
	}
	if result.AfterResponse == nil {
		t.Fatal("restart must schedule via AfterResponse")
	}
	if err := result.AfterResponse(ctx); err != nil {
		t.Fatalf("AfterResponse: %v", err)
	}
	if bot.Accepting() {
		t.Fatal("bot should not accept after restart scheduled")
	}
	if !bot.RestartRequested() {
		t.Fatal("RestartRequested should be true")
	}
}

func TestDispatchRestartNewQueueReplacesStoredQueue(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	ctx := context.Background()
	if err := bot.SaveEventQueueStateForTest(ctx, zulipbot.QueueState{
		QueueID:     "old-queue",
		LastEventID: 123,
	}); err != nil {
		t.Fatalf("SaveEventQueueStateForTest: %v", err)
	}

	result := bot.Dispatch(ctx, ownerRequest("restart", "--new-queue"))
	if !strings.Contains(result.Content, "register a new one") {
		t.Fatalf("restart reply = %q", result.Content)
	}
	if result.AfterResponse == nil {
		t.Fatal("restart must schedule via AfterResponse")
	}
	if err := result.AfterResponse(ctx); err != nil {
		t.Fatalf("AfterResponse: %v", err)
	}

	state, ok, err := bot.EventQueueStateForTest(ctx)
	if err != nil {
		t.Fatalf("EventQueueStateForTest: %v", err)
	}
	if !ok {
		t.Fatal("event queue state should exist")
	}
	if state.QueueID == "old-queue" {
		t.Fatalf("event queue was not replaced: %+v", state)
	}
	if state.LastEventID != 0 {
		t.Fatalf("new queue should start from returned last event id, got %+v", state)
	}
	if !bot.RestartRequested() {
		t.Fatal("RestartRequested should be true")
	}
}

func TestDispatchRestartRejectsUnknownOption(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), ownerRequest("restart", "--bogus"))
	if !strings.Contains(result.Content, "Unknown restart option") {
		t.Fatalf("restart reply = %q", result.Content)
	}
	if result.AfterResponse != nil {
		t.Fatal("invalid restart option must not schedule restart")
	}
	if bot.RestartRequested() {
		t.Fatal("RestartRequested should be false")
	}
}

func TestDispatchRefusesWhenNotAccepting(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	bot.SetAcceptingForTest(false)

	result := bot.Dispatch(context.Background(), memberRequest("help"))
	if !strings.Contains(result.Content, "restarting") {
		t.Fatalf("expected restarting refusal, got %q", result.Content)
	}
}

func TestDispatchUnknownCommand(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), memberRequest("bogus"))
	if !strings.Contains(result.Content, `Unknown command "bogus"`) {
		t.Fatalf("unknown command reply = %q", result.Content)
	}
}

func TestRegisterQueueRequestsReactionEvents(t *testing.T) {
	t.Parallel()

	client := zulipmock.NewClient()
	client.SetOwnUser(zulip.User{UserID: 100, Email: "bot@example.com", FullName: "Mock Bot", IsBot: true})
	client.AddUser(zulip.User{UserID: 100, IsBot: true})

	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	db, queries := openZulipbotTestStorage(t, dbPath)
	bot, err := zulipbot.NewBot(
		context.Background(),
		zulipbot.RuntimeConfig{Logger: slog.Default()},
		client,
		db,
		queries,
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	if _, err = bot.RegisterQueueForTest(context.Background()); err != nil {
		t.Fatalf("RegisterQueueForTest: %v", err)
	}

	got := client.RegisterEventTypes()
	want := []events.EventType{
		events.EventTypeHeartbeat,
		events.EventTypeMessage,
		events.EventTypeReaction,
	}
	if len(got) != len(want) {
		t.Fatalf("registered event types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registered event types = %v, want %v", got, want)
		}
	}
}

func TestHandleReactionSubscribesForNonUnicodeMappedEmoji(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	client := zulipmock.NewClient()
	client.SetOwnUser(zulip.User{UserID: 100, Email: "bot@example.com", FullName: "Mock Bot", IsBot: true})
	client.AddUser(zulip.User{UserID: 100, IsBot: true})

	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	db, queries := openZulipbotTestStorage(t, dbPath)
	bot, err := zulipbot.NewBot(
		ctx,
		zulipbot.RuntimeConfig{Logger: slog.Default()},
		client,
		db,
		queries,
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	const messageID = int64(4242)
	const userID = int64(7)
	const channelGroupID = int64(99)
	if err = queries.SaveAnnouncementState(ctx, storagedb.SaveAnnouncementStateParams{
		MessageID: sql.NullInt64{Int64: messageID, Valid: true},
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("SaveAnnouncementState: %v", err)
	}
	if err = queries.UpsertEmojiGroupMapping(ctx, storagedb.UpsertEmojiGroupMappingParams{
		ChannelGroupID: channelGroupID,
		EmojiName:      "zero",
		Enabled:        1,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		UpdatedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping: %v", err)
	}

	subscriber := &recordingGroupSubscriber{}
	bot.SetGroupSubscriberForTest(subscriber)

	event := decodeReactionEvent(t, `{
		"id": 6,
		"type": "reaction",
		"op": "add",
		"message_id": 4242,
		"user_id": 7,
		"emoji_name": "zero",
		"emoji_code": "0030-20e3",
		"reaction_type": "zulip_extra_emoji"
	}`)
	if err = bot.HandleReaction(ctx, event); err != nil {
		t.Fatalf("HandleReaction: %v", err)
	}

	if subscriber.subscribedUserID != userID || subscriber.subscribedGroupID != channelGroupID {
		t.Fatalf(
			"SubscribeUser called with user=%d group=%d, want user=%d group=%d",
			subscriber.subscribedUserID,
			subscriber.subscribedGroupID,
			userID,
			channelGroupID,
		)
	}
}

func TestDispatchChainHonorsShellLikeOperators(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	chain, err := command.ParseChain("bogus && status || help ; status")
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}

	result := bot.DispatchChain(context.Background(), memberRequest(""), chain)
	if !strings.Contains(result.Content, `Unknown command "bogus"`) {
		t.Fatalf("chain should include failed first command: %q", result.Content)
	}
	if !strings.Contains(result.Content, "Supported commands") {
		t.Fatalf("chain should run || fallback: %q", result.Content)
	}
	if got := strings.Count(result.Content, "Bot status:"); got != 1 {
		t.Fatalf("chain should skip && status and run ; status once, got %d in %q", got, result.Content)
	}
}

func TestDispatchChainSkipsOrAfterSuccess(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	chain, err := command.ParseChain("status || bogus && help")
	if err != nil {
		t.Fatalf("ParseChain: %v", err)
	}

	result := bot.DispatchChain(context.Background(), memberRequest(""), chain)
	if !strings.Contains(result.Content, "Bot status:") {
		t.Fatalf("chain should run status: %q", result.Content)
	}
	if strings.Contains(result.Content, "Unknown command") {
		t.Fatalf("chain should skip || command after success: %q", result.Content)
	}
	if !strings.Contains(result.Content, "Supported commands") {
		t.Fatalf("chain should run && command after success: %q", result.Content)
	}
}

func TestHandleMessageSendsTypingStartAndStopAroundCommand(t *testing.T) {
	t.Parallel()

	client := zulipmock.NewClient()
	client.SetOwnUser(zulip.User{UserID: 100, Email: "bot@example.com", FullName: "Mock Bot", IsBot: true})
	client.AddUser(zulip.User{UserID: 7, Role: zulip.RoleMember})

	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	db, queries := openZulipbotTestStorage(t, dbPath)
	bot, err := zulipbot.NewBot(
		context.Background(),
		zulipbot.RuntimeConfig{Logger: slog.Default()},
		client,
		db,
		queries,
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	userID := int64(7)
	botID := int64(100)
	err = bot.HandleMessage(context.Background(), events.MessageEvent{
		Message: zulip.Message{
			ID:       1234,
			Content:  "bogus",
			SenderID: userID,
			Type:     zulip.RecipientTypeDirect,
			DisplayRecipient: zulip.DisplayRecipientFromUserRecipentArray([]zulip.UserRecipent{
				{ID: &userID},
				{ID: &botID},
			}),
		},
	})
	if err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	statuses := client.TypingStatuses()
	if len(statuses) != 2 {
		t.Fatalf("typing status count = %d, want 2: %#v", len(statuses), statuses)
	}
	if statuses[0].Op != zulip.TypingStatusOpStart {
		t.Fatalf("first typing op = %q, want start", statuses[0].Op)
	}
	if statuses[1].Op != zulip.TypingStatusOpStop {
		t.Fatalf("second typing op = %q, want stop", statuses[1].Op)
	}
	for i, status := range statuses {
		if len(status.Recipient.Users) != 1 || status.Recipient.Users[0] != userID {
			t.Fatalf("typing status %d recipient = %#v, want user %d", i, status.Recipient, userID)
		}
		if status.RecipientType == nil || *status.RecipientType != zulip.RecipientTypeDirect {
			t.Fatalf("typing status %d recipient type = %v, want direct", i, status.RecipientType)
		}
	}
}
