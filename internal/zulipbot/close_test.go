package zulipbot_test

import (
	"context"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
)

func TestCloseKeepsStoredQueue(t *testing.T) {
	t.Parallel()

	bot := openBotWithStoredQueue(t)
	if err := bot.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	assertStoredQueueKept(t, bot)
}

func TestCloseKeepsStoredQueueAfterFailedRun(t *testing.T) {
	t.Parallel()

	bot := openBotWithStoredQueue(t)
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := bot.Run(expired); err == nil {
		t.Fatal("Run() should fail with an expired context")
	}
	if err := bot.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
	assertStoredQueueKept(t, bot)
}

func openBotWithStoredQueue(t *testing.T) *zulipbot.Bot {
	t.Helper()

	bot, _ := openRestartTestBot(t, restartTestDBPath(t))
	if err := bot.SaveEventQueueStateForTest(context.Background(), zulipbot.QueueState{
		QueueID:     "stored-queue",
		LastEventID: 42,
	}); err != nil {
		t.Fatalf("SaveEventQueueStateForTest: %v", err)
	}
	return bot
}

func assertStoredQueueKept(t *testing.T, bot *zulipbot.Bot) {
	t.Helper()

	state, ok, err := bot.EventQueueStateForTest(context.Background())
	if err != nil {
		t.Fatalf("EventQueueStateForTest: %v", err)
	}
	if !ok || state.QueueID != "stored-queue" || state.LastEventID != 42 {
		t.Fatalf("queue state after Close = %+v, %v; want stored-queue@42", state, ok)
	}
}
