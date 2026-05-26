//go:build integration

// Package integration_test contains channel-group integration tests that
// require a live Zulip server.
//
// Run with:
//
//	go test -v -tags integration ./internal/channelgroup/integration/ -timeout 90s
//
// Required environment variables:
//
//	CAMPUSBOT_INTEGRATION_ZULIPRC  - path to a zuliprc for a Zulip bot account on a test realm
package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
)

func requireZulipRC(t *testing.T) string {
	t.Helper()
	rcPath := os.Getenv("CAMPUSBOT_INTEGRATION_ZULIPRC")
	if rcPath == "" {
		t.Skip("CAMPUSBOT_INTEGRATION_ZULIPRC not set; skipping integration tests")
	}
	return rcPath
}

func TestIntegration_ChannelDeleteRemovesChannelFromChannelGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	base := newZulipClient(t)
	ownUserID := ownUserID(t, ctx, base)
	database := newChannelGroupDatabase(t)
	client := newInitializedChannelGroupClient(t, ctx, base, database)

	channelID := createIntegrationChannel(t, ctx, base, ownUserID, "delete")
	archived := false
	t.Cleanup(func() {
		if !archived {
			_, _, _ = base.ArchiveChannel(context.Background(), channelID).Execute()
		}
	})

	created, _, err := client.CreateChannelGroup(ctx).
		Name(integrationName("cg-channel-delete")).
		ChannelIDs([]int64{channelID}).
		InitialSubscribers(zulip.UserIDsAsPrincipals(ownUserID)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup failed: %v", err)
	}
	defer deactivateUserGroup(context.Background(), base, created.ChannelGroupID)

	if _, _, err = base.ArchiveChannel(ctx, channelID).Execute(); err != nil {
		t.Fatalf("ArchiveChannel failed: %v", err)
	}
	archived = true

	waitFor(t, ctx, func() (bool, error) {
		resp, _, err := client.GetChannelGroupChannels(ctx, created.ChannelGroupID).Execute()
		if err != nil {
			return false, err
		}
		return len(resp.ChannelIDs) == 0, nil
	})
}

func TestIntegration_UserGroupDeleteRemovesChannelGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	base := newZulipClient(t)
	ownUserID := ownUserID(t, ctx, base)
	database := newChannelGroupDatabase(t)
	client := newInitializedChannelGroupClient(t, ctx, base, database)

	created, _, err := client.CreateChannelGroup(ctx).
		Name(integrationName("cg-usergroup-delete")).
		InitialSubscribers(zulip.UserIDsAsPrincipals(ownUserID)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup failed: %v", err)
	}

	if _, _, err = base.DeactivateUserGroup(ctx, created.ChannelGroupID).Execute(); err != nil {
		t.Fatalf("DeactivateUserGroup failed: %v", err)
	}

	waitFor(t, ctx, func() (bool, error) {
		_, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
		if err == nil {
			return false, nil
		}
		return true, nil
	})
}

func newZulipClient(t *testing.T) zulipclient.Client {
	t.Helper()

	rc, err := zulip.NewZulipRCFromFile(requireZulipRC(t))
	if err != nil {
		t.Fatalf("load zuliprc: %v", err)
	}
	client, err := zulipclient.NewClient(rc, zulipclient.WithClientName("go-campusbot-channelgroup-integration"))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return client
}

func ownUserID(t *testing.T, ctx context.Context, client zulipclient.Client) int64 {
	t.Helper()

	resp, _, err := client.GetOwnUser(ctx).Execute()
	if err != nil {
		t.Fatalf("GetOwnUser failed: %v", err)
	}
	if resp == nil || resp.User.UserID == 0 {
		t.Fatal("GetOwnUser returned no user ID")
	}
	return resp.User.UserID
}

func newChannelGroupDatabase(t *testing.T) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	schema, err := os.ReadFile("../db/schema.sql")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if _, err = database.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return database
}

func newInitializedChannelGroupClient(
	t *testing.T,
	ctx context.Context,
	base zulipclient.Client,
	database *sql.DB,
) channelgroup.Client {
	t.Helper()

	client, err := channelgroup.NewInitializedClient(
		ctx,
		base,
		database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewInitializedClient failed: %v", err)
	}
	return client
}

func createIntegrationChannel(
	t *testing.T,
	ctx context.Context,
	client zulipclient.Client,
	ownUserID int64,
	suffix string,
) int64 {
	t.Helper()

	resp, _, err := client.CreateChannel(ctx).
		Name(integrationName("cg-" + suffix)).
		Description("go-campusbot channelgroup integration test").
		Subscribers([]int64{ownUserID}).
		SendNewSubscriptionMessages(false).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannel failed: %v", err)
	}
	if resp == nil || resp.ID == 0 {
		t.Fatal("CreateChannel returned no channel ID")
	}
	return resp.ID
}

func integrationName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func deactivateUserGroup(ctx context.Context, client zulipclient.Client, groupID int64) {
	_, _, _ = client.DeactivateUserGroup(ctx, groupID).Execute()
}

func waitFor(t *testing.T, ctx context.Context, condition func() (bool, error)) {
	t.Helper()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		ok, err := condition()
		if err != nil {
			t.Fatalf("condition failed: %v", err)
		}
		if ok {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("condition not met before timeout: %v", contextCause(ctx))
		case <-ticker.C:
		}
	}
}

func contextCause(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ctx.Err()
}
