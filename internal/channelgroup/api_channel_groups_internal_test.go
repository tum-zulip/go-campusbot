package channelgroup

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/tum-zulip/go-zulip/zulip/events"

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
		WithLogger(slog.New(slog.DiscardHandler)),
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

func TestChannelArchiveEventsRemoveChannelFromChannelGroups(t *testing.T) {
	t.Parallel()

	archived := true
	for name, archiveEvent := range map[string]func(channelID int64) events.Event{
		"delete": func(channelID int64) events.Event {
			return events.ChannelDeleteEvent{ChannelIDs: []int64{channelID}}
		},
		"update is_archived": func(channelID int64) events.Event {
			return events.ChannelUpdateEvent{
				ChannelID: channelID,
				Property:  "is_archived",
				Value:     &events.ChannelEventUpdateValue{Bool: &archived},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			base := zulipmock.NewClient()
			service := newInternalTestService(t, base)

			created, _, err := base.CreateUserGroup(ctx).Name("SIX").Description("").Members([]int64{1}).Execute()
			if err != nil {
				t.Fatalf("CreateUserGroup: %v", err)
			}
			if err := service.ImportZulipUserGroup(ctx, created.GroupID); err != nil {
				t.Fatalf("ImportZulipUserGroup: %v", err)
			}
			channel, _, err := base.CreateChannel(ctx).Name("IN0001").Execute()
			if err != nil {
				t.Fatalf("CreateChannel: %v", err)
			}
			if _, _, err := service.UpdateChannelGroupChannels(ctx, created.GroupID).
				Add([]int64{channel.ID}).
				Execute(); err != nil {
				t.Fatalf("UpdateChannelGroupChannels: %v", err)
			}

			if err := service.handleChannelGroupEvent(ctx, archiveEvent(channel.ID)); err != nil {
				t.Fatalf("handleChannelGroupEvent: %v", err)
			}

			group, err := service.getGroup(ctx, created.GroupID)
			if err != nil {
				t.Fatalf("getGroup: %v", err)
			}
			if len(group.ChannelIDs) != 0 {
				t.Fatalf("channel IDs = %v, want archived channel removed", group.ChannelIDs)
			}
		})
	}
}

func TestUserGroupDeactivationEventRemovesChannelGroup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	base := zulipmock.NewClient()
	service := newInternalTestService(t, base)

	created, _, err := base.CreateUserGroup(ctx).Name("SIX").Description("").Members([]int64{1}).Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup: %v", err)
	}
	if err := service.ImportZulipUserGroup(ctx, created.GroupID); err != nil {
		t.Fatalf("ImportZulipUserGroup: %v", err)
	}
	if _, _, err := base.DeactivateUserGroup(ctx, created.GroupID).Execute(); err != nil {
		t.Fatalf("DeactivateUserGroup: %v", err)
	}

	deactivated := true
	event := events.UserGroupUpdateEvent{
		GroupID: created.GroupID,
		Data:    events.UserGroupUpdateData{Deactivated: &deactivated},
	}
	if err := service.handleChannelGroupEvent(ctx, event); err != nil {
		t.Fatalf("handleChannelGroupEvent: %v", err)
	}

	if _, err := service.getGroup(ctx, created.GroupID); !errors.Is(err, ErrChannelGroupNotFound) {
		t.Fatalf("getGroup error = %v, want ErrChannelGroupNotFound", err)
	}
}
