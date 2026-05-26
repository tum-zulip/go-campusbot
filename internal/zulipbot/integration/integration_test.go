//go:build integration

// Package integration_test contains integration tests that require a live Zulip server.
//
// Run with:
//
//	go test -v -tags integration ./internal/zulipbot/integration/ -timeout 30s
//
// Required environment variables:
//
//	CAMPUSBOT_INTEGRATION_ZULIPRC  - path to a zuliprc for a Zulip bot account on a test realm
//
// Note: the credentials must belong to a Zulip bot account (not a regular user account).
// The bot owner is resolved automatically from the Zulip API via bot_owner_id.
package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/eventloop"
)

func requireZulipRC(t *testing.T) string {
	t.Helper()
	rcPath := os.Getenv("CAMPUSBOT_INTEGRATION_ZULIPRC")
	if rcPath == "" {
		t.Skip("CAMPUSBOT_INTEGRATION_ZULIPRC not set; skipping integration tests")
	}
	return rcPath
}

// TestIntegration_QueueRegistrationAndCleanup verifies that NewApp succeeds and
// the bot user info is populated.
func TestIntegration_QueueRegistrationAndCleanup(t *testing.T) {
	rcPath := requireZulipRC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	dbPath := t.TempDir() + "/integration.sqlite3"

	app, err := zulipbot.NewApp(ctx, zulipbot.RuntimeConfig{
		RCPath: rcPath,
		DBPath: dbPath,
	})
	if err != nil {
		t.Fatalf("NewApp failed: %v", err)
	}
	defer func() {
		if err := app.Close(); err != nil {
			t.Logf("app.Close: %v", err)
		}
	}()

	ownUser := app.Bot().OwnUser()
	if ownUser.UserID == 0 {
		t.Fatal("expected non-zero bot user ID")
	}
	t.Logf("bot user: user_id=%d email=%s", ownUser.UserID, ownUser.Email)
}

// TestIntegration_EventQueueRegisterAndCheck verifies that a Zulip event queue
// can be registered and validated with the live server.
func TestIntegration_EventQueueRegisterAndCheck(t *testing.T) {
	rcPath := requireZulipRC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	bot, err := zulipbot.New(ctx, zulipbot.Config{RCPath: rcPath})
	if err != nil {
		t.Fatalf("New bot failed: %v", err)
	}

	source := eventloop.NewZulipSource(bot.Client())

	state, err := source.Register(ctx, eventloop.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	t.Logf("registered queue: %s last_event_id=%d", state.QueueID, state.LastEventID)

	if state.QueueID == "" {
		t.Fatal("expected non-empty queue ID")
	}

	// Check (non-blocking) to verify queue is live.
	if err := source.Check(ctx, state); err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	t.Log("queue check passed")

	// Delete the queue to clean up.
	if err := source.Delete(ctx, state.QueueID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	t.Log("queue deleted successfully")
}
