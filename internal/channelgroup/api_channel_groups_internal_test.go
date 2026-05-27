package channelgroup

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

func newInternalTestService(t *testing.T, base zulipmock.Client) *channelGroups {
	t.Helper()

	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	schema, err := os.ReadFile("db/sql/schema.sql")
	if err != nil {
		t.Fatalf("read channelgroup schema: %v", err)
	}
	if _, err := database.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply channelgroup schema: %v", err)
	}

	return newChannelGroups(
		base,
		database,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
}

func TestRemoveDeletedUserGroupChannelGroupIgnoresStaleEventForActiveGroup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	base := zulipmock.NewClient()
	service := newInternalTestService(t, base)

	created, _, err := base.CreateUserGroup(ctx).
		Name("SIX").
		Description("").
		Members([]int64{1}).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup: %v", err)
	}
	if err := service.ImportZulipUserGroup(ctx, created.GroupID); err != nil {
		t.Fatalf("ImportZulipUserGroup: %v", err)
	}

	if err := service.removeDeletedUserGroupChannelGroup(ctx, created.GroupID); err != nil {
		t.Fatalf("removeDeletedUserGroupChannelGroup: %v", err)
	}

	if _, err := service.getGroup(ctx, created.GroupID); err != nil {
		t.Fatalf("active channel group was deleted by stale event: %v", err)
	}
}
