package zulipbot_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
	"github.com/tum-zulip/go-zulip/zulip"
)

func TestScheduleRestartStopsAcceptingCommands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := restartTestDBPath(t)
	bot, _ := openRestartTestBot(t, dbPath)

	if !bot.Accepting() {
		t.Fatal("app should be accepting before restart is scheduled")
	}

	_, _, err := bot.ScheduleRestart(
		ctx,
		command.Actor{UserID: 1},
		10,
		command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{1}},
	)
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}

	if bot.Accepting() {
		t.Fatal("app should stop accepting after restart is scheduled")
	}
	if !bot.RestartRequested() {
		t.Fatal("restart should be requested")
	}
}

func TestDoubleScheduleRestartIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := restartTestDBPath(t)
	bot, _ := openRestartTestBot(t, dbPath)
	target := command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{1}}

	_, first, err := bot.ScheduleRestart(ctx, command.Actor{UserID: 1}, 1, target)
	if err != nil {
		t.Fatalf("first ScheduleRestart() failed: %v", err)
	}
	if !first {
		t.Fatal("first ScheduleRestart() should return scheduled=true")
	}

	_, second, err := bot.ScheduleRestart(ctx, command.Actor{UserID: 1}, 2, target)
	if err != nil {
		t.Fatalf("second ScheduleRestart() failed: %v", err)
	}
	if second {
		t.Fatal("second ScheduleRestart() should return scheduled=false")
	}
}

func TestNotifyRestartCompleteCompletesPendingRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	target := command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{10}}

	dbPath := restartTestDBPath(t)
	bot, _ := openRestartTestBot(t, dbPath)
	id, _, err := bot.ScheduleRestart(ctx, command.Actor{UserID: 10}, 55, target)
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}

	bot, client := openRestartTestBot(t, dbPath)
	if notifyErr := bot.NotifyRestartComplete(ctx); notifyErr != nil {
		t.Fatalf("NotifyRestartComplete() failed: %v", notifyErr)
	}
	if got := client.LastSentMessage(); got == nil || len(got.Recipient.Users) == 0 || got.Recipient.Users[0] != 10 {
		t.Fatalf("sent target = %#v", got)
	}
	if ok, err := bot.RestartPending(ctx); err != nil {
		t.Fatalf("RestartPending() failed: %v", err)
	} else if ok {
		t.Fatalf("restart request %d should be completed", id)
	}
}

func openRestartTestBot(t *testing.T, dbPath string) (*zulipbot.Bot, zulipmock.Client) {
	t.Helper()

	client := zulipmock.NewClient()
	client.SetOwnUser(zulip.User{UserID: 100, Email: "bot@example.com", FullName: "Mock Bot", IsBot: true})
	botOwnerID := int64(99)
	client.AddUser(zulip.User{UserID: 100, IsBot: true, BotOwnerID: &botOwnerID})
	repo, err := storage.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("storage.Open() failed: %v", err)
	}

	bot, err := zulipbot.NewBot(
		context.Background(),
		zulipbot.RuntimeConfig{Logger: slog.Default()},
		client,
		repo,
	)
	if err != nil {
		_ = repo.Close()
		t.Fatalf("NewBot() failed: %v", err)
	}
	t.Cleanup(func() { _ = bot.Close() })
	return bot, client
}

func restartTestDBPath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), fmt.Sprintf("%s.sqlite3", t.Name()))
}
