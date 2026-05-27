package zulipbot_test

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

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
