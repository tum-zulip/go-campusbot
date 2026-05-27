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
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
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
	client := newIntegrationClient(t, rcPath)
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	if err := storagedb.ConfigureSQLite(ctx, db); err != nil {
		_ = db.Close()
		t.Fatalf("ConfigureSQLite failed: %v", err)
	}
	if err := storagedb.InitSchema(ctx, db); err != nil {
		_ = db.Close()
		t.Fatalf("InitSchema failed: %v", err)
	}
	queries := storagedb.New(db)

	bot, err := zulipbot.NewBot(ctx, zulipbot.RuntimeConfig{}, client, db, queries)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewBot failed: %v", err)
	}
	defer func() {
		if err := bot.Close(); err != nil {
			t.Logf("bot.Close: %v", err)
		}
		if err := db.Close(); err != nil {
			t.Logf("db.Close: %v", err)
		}
	}()

	ownUser := bot.OwnUser()
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

	client := newIntegrationClient(t, rcPath)
	bot, err := zulipbot.New(ctx, client)
	if err != nil {
		t.Fatalf("New bot failed: %v", err)
	}

	source := zulipbot.NewZulipSource(bot.Client())

	state, err := source.Register(ctx, zulipbot.RegisterOptions{})
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

func newIntegrationClient(t *testing.T, rcPath string) zulipclient.Client {
	t.Helper()

	rc, err := zulip.NewZulipRCFromFile(rcPath)
	if err != nil {
		t.Fatalf("load zuliprc: %v", err)
	}
	client, err := zulipclient.NewClient(rc, zulipclient.WithClientName(zulipbot.DefaultClientName))
	if err != nil {
		t.Fatalf("create Zulip client: %v", err)
	}
	return client
}
