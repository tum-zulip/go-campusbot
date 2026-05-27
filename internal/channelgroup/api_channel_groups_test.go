package channelgroup_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
)

func newTestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err = database.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	schema, err := os.ReadFile("db/sql/schema.sql")
	if err != nil {
		t.Fatalf("read channelgroup schema: %v", err)
	}
	if _, err = database.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply channelgroup schema: %v", err)
	}
	return database
}

func TestMigrateAddsChannelFolderColumnToExistingChannelGroups(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err = database.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	if _, err := database.ExecContext(ctx, `
		CREATE TABLE channel_groups (
			id INTEGER PRIMARY KEY
		);
	`); err != nil {
		t.Fatalf("seed old channelgroup schema: %v", err)
	}

	if err := channelgroup.Migrate(ctx, database); err != nil {
		t.Fatalf("Migrate() failed: %v", err)
	}

	rows, err := database.QueryContext(ctx, "PRAGMA table_info(channel_groups)")
	if err != nil {
		t.Fatalf("inspect migrated schema: %v", err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan migrated schema: %v", err)
		}
		if name == "channel_folder_id" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migrated schema: %v", err)
	}
	if !found {
		t.Fatal("channel_folder_id column was not added")
	}
}

func newTestClient(t *testing.T, base zulipmock.Client) channelgroup.Client {
	t.Helper()

	database := newTestDatabase(t)
	client, err := channelgroup.NewClient(
		context.Background(),
		base,
		database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}
	return client
}

func TestCreateChannelGroupWithMockClient(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t, zulipmock.NewClient())

	created, _, err := client.CreateChannelGroup(ctx).
		Name("course group").
		InitialSubscribers(zulip.UserIDsAsPrincipals(20, 10)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}

	if group.ChannelGroup.Name != "course group" {
		t.Fatalf("name = %q, want %q", group.ChannelGroup.Name, "course group")
	}
	if group.ChannelGroup.ID != created.ChannelGroupID {
		t.Fatalf("id = %d, want %d", group.ChannelGroup.ID, created.ChannelGroupID)
	}
	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{10, 20}; !equalInt64s(got, want) {
		t.Fatalf("subscribers = %v, want %v", got, want)
	}
}

func TestCreateChannelGroupRollsBackLocalDBWritesOnChannelInsertFailure(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite database: %v", err)
	}
	database.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err = database.Close(); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	if _, err = database.ExecContext(ctx, `
		CREATE TABLE channel_groups (
			id INTEGER PRIMARY KEY,
			channel_folder_id INTEGER
		);
	`); err != nil {
		t.Fatalf("seed partial channelgroup schema: %v", err)
	}

	client, err := channelgroup.NewClient(
		ctx,
		zulipmock.NewClient(),
		database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}

	if _, _, err = client.CreateChannelGroup(ctx).
		Name("rollback local db write").
		ChannelIDs([]int64{101}).
		Execute(); err == nil {
		t.Fatal("CreateChannelGroup error = nil, want local channel insert failure")
	}

	var groupCount int
	if err = database.QueryRowContext(ctx, "SELECT COUNT(*) FROM channel_groups").Scan(&groupCount); err != nil {
		t.Fatalf("count channel_groups: %v", err)
	}
	if groupCount != 0 {
		t.Fatalf("channel_groups count = %d, want 0 after rollback", groupCount)
	}
}

func TestCreateChannelGroupCreatesUserGroup(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("restricted group").
		InitialSubscribers(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	groups, _, err := base.GetUserGroups(ctx).Execute()
	if err != nil {
		t.Fatalf("GetUserGroups error = %v", err)
	}
	group, ok := findUserGroupByID(groups.UserGroups, created.ChannelGroupID)
	if !ok {
		t.Fatalf("created user group %d not found in %+v", created.ChannelGroupID, groups.UserGroups)
	}
	if group.ID != created.ChannelGroupID {
		t.Fatalf("user group ID = %d, want %d", group.ID, created.ChannelGroupID)
	}
	assertAdminGroupSettingValue(t, "CanAddMembersGroup", group.CanAddMembersGroup)
	assertAdminGroupSettingValue(t, "CanJoinGroup", group.CanJoinGroup)
	assertAdminGroupSettingValue(t, "CanLeaveGroup", group.CanLeaveGroup)
	assertAdminGroupSettingValue(t, "CanManageGroup", group.CanManageGroup)
	assertAdminGroupSettingValue(t, "CanMentionGroup", group.CanMentionGroup)
	assertAdminGroupSettingValue(t, "CanRemoveMembersGroup", group.CanRemoveMembersGroup)
}

func TestCreateChannelGroupRollsBackUserGroupOnError(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 1)
	base.FailNext(zulipmock.OperationSubscribe, errors.New("subscribe failed"))

	_, _, err := client.CreateChannelGroup(ctx).
		Name("rollback group").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err == nil {
		t.Fatalf("CreateChannelGroup error = nil, want failure")
	}

	groups, _, err := base.GetUserGroups(ctx).Execute()
	if err != nil {
		t.Fatalf("GetUserGroups error = %v", err)
	}
	rolledBackGroup, ok := findUserGroupByName(groups.UserGroups, "rollback group")
	if !ok {
		t.Fatalf("rolled-back user group not found in %+v", groups.UserGroups)
	}
	if !rolledBackGroup.Deactivated {
		t.Fatalf("created user group was not deactivated after rollback")
	}

	_, _, err = client.GetChannelGroup(ctx, rolledBackGroup.ID).Execute()
	if !errors.Is(err, channelgroup.ErrChannelGroupNotFound) {
		t.Fatalf("GetChannelGroup error = %v, want ErrChannelGroupNotFound", err)
	}
}

func TestChannelGroupWithChannelFolderAssignsInitialAndAddedChannels(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 3)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder group").
		ChannelIDs(channelIDs[:1]).
		CreateChannelFolder(true).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatalf("channel folder ID = nil, want created folder")
	}

	folders, _, err := base.GetChannelFolders(ctx).Execute()
	if err != nil {
		t.Fatalf("GetChannelFolders error = %v", err)
	}
	if len(folders.ChannelFolders) != 1 {
		t.Fatalf("channel folders = %d, want 1", len(folders.ChannelFolders))
	}
	if folders.ChannelFolders[0].Name != "folder group" {
		t.Fatalf("channel folder name = %q, want %q", folders.ChannelFolders[0].Name, "folder group")
	}

	assertChannelFolderID(t, ctx, base, channelIDs[0], *group.ChannelGroup.ChannelFolderID)
	assertNoChannelFolder(t, ctx, base, channelIDs[1])

	_, _, err = client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
		Add(channelIDs[1:]).
		Execute()
	if err != nil {
		t.Fatalf("UpdateChannelGroupChannels error = %v", err)
	}
	assertChannelFolderID(t, ctx, base, channelIDs[1], *group.ChannelGroup.ChannelFolderID)
	assertChannelFolderID(t, ctx, base, channelIDs[2], *group.ChannelGroup.ChannelFolderID)
}

//nolint:funlen
func TestChannelGroupFolderLifecycle(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder lifecycle").
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute(); err != nil {
		t.Fatalf("Add folder error = %v", err)
	}
	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after add")
	}
	folderID := *group.ChannelGroup.ChannelFolderID
	assertChannelFolderID(t, ctx, base, channelIDs[0], folderID)
	assertChannelFolderID(t, ctx, base, channelIDs[1], folderID)
	assertChannelFolderArchived(t, ctx, base, folderID, false)

	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Unassign().Execute(); err != nil {
		t.Fatalf("Unassign folder error = %v", err)
	}
	group, _, err = client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup after unassign error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil || *group.ChannelGroup.ChannelFolderID != folderID {
		t.Fatalf("channel folder ID after unassign = %v, want %d", group.ChannelGroup.ChannelFolderID, folderID)
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertChannelFolderArchived(t, ctx, base, folderID, false)

	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Assign().Execute(); err != nil {
		t.Fatalf("Assign folder error = %v", err)
	}
	assertChannelFolderID(t, ctx, base, channelIDs[0], folderID)
	assertChannelFolderID(t, ctx, base, channelIDs[1], folderID)

	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Remove().Execute(); err != nil {
		t.Fatalf("Remove folder error = %v", err)
	}
	group, _, err = client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup after remove error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID != nil {
		t.Fatalf("channel folder ID after remove = %d, want nil", *group.ChannelGroup.ChannelFolderID)
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertChannelFolderArchived(t, ctx, base, folderID, true)
}

func TestChannelGroupFolderRemoveRejectsManualChannelInFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 3)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder manual member").
		ChannelIDs(channelIDs[:2]).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}
	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute(); err != nil {
		t.Fatalf("Add folder error = %v", err)
	}
	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after add")
	}
	folderID := *group.ChannelGroup.ChannelFolderID
	if _, _, err = base.UpdateChannel(ctx, channelIDs[2]).FolderID(folderID).Execute(); err != nil {
		t.Fatalf("move manual channel to group folder: %v", err)
	}

	_, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Remove().Execute()
	var externalChannel channelgroup.ChannelFolderExternalChannelError
	if !errors.As(err, &externalChannel) {
		t.Fatalf("expected ChannelFolderExternalChannelError, got %T: %v", err, err)
	}
	if externalChannel.ChannelID != channelIDs[2] || externalChannel.ChannelFolderID != folderID {
		t.Fatalf("unexpected external channel error: %+v", externalChannel)
	}

	assertChannelFolderID(t, ctx, base, channelIDs[0], folderID)
	assertChannelFolderID(t, ctx, base, channelIDs[1], folderID)
	assertChannelFolderID(t, ctx, base, channelIDs[2], folderID)
	assertChannelFolderArchived(t, ctx, base, folderID, false)
}

func TestChannelGroupFolderUnassignRejectsChannelInDifferentFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder conflict").
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}
	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute(); err != nil {
		t.Fatalf("Add folder error = %v", err)
	}

	otherFolder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder error = %v", err)
	}
	if _, _, err = base.UpdateChannel(ctx, channelIDs[0]).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("move channel to other folder: %v", err)
	}

	_, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Unassign().Execute()
	var conflict channelgroup.ChannelFolderConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ChannelFolderConflictError, got %T: %v", err, err)
	}
	if conflict.ChannelID != channelIDs[0] || conflict.ConflictingFolderID != otherFolder.ChannelFolderID {
		t.Fatalf("unexpected conflict: %+v", conflict)
	}
	assertChannelFolderID(t, ctx, base, channelIDs[0], otherFolder.ChannelFolderID)
}

func TestChannelGroupFolderAddRejectsChannelInDifferentFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder add conflict").
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}
	otherFolder, _, err := base.CreateChannelFolder(ctx).Name("manual folder").Execute()
	if err != nil {
		t.Fatalf("CreateChannelFolder error = %v", err)
	}
	if _, _, err = base.UpdateChannel(ctx, channelIDs[0]).FolderID(otherFolder.ChannelFolderID).Execute(); err != nil {
		t.Fatalf("move channel to other folder: %v", err)
	}

	_, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute()
	var conflict channelgroup.ChannelFolderConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ChannelFolderConflictError, got %T: %v", err, err)
	}
	assertChannelFolderID(t, ctx, base, channelIDs[0], otherFolder.ChannelFolderID)
}

func TestConcurrentFolderAddAndUnassignSerializesToUnassignedFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder add unassign race").
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroups),
		zulipmock.OperationRequest(zulipmock.OperationGetSubscribers),
	)
	runStartedBeforePreviousCompletes(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Unassign().Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil, want folder kept after unassign")
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertChannelFolderArchived(t, ctx, base, *group.ChannelGroup.ChannelFolderID, false)
}

func TestConcurrentFolderAddAndRemoveSerializesToRemovedFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("folder add remove race").
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroups),
		zulipmock.OperationRequest(zulipmock.OperationGetSubscribers),
	)
	runStartedBeforePreviousCompletes(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Remove().Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID != nil {
		t.Fatalf("channel folder ID = %d, want nil after remove", *group.ChannelGroup.ChannelFolderID)
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertOnlyChannelFolderArchived(t, ctx, base, true)
}

func TestConcurrentFolderAssignAndUnassignSerializesToUnassignedFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)
	created, folderID := createGroupWithUnassignedFolder(
		t,
		ctx,
		client,
		base,
		channelIDs,
		"folder assign unassign race",
	)

	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.OperationRequest(zulipmock.OperationGetSubscribers),
	)
	runStartedBeforePreviousCompletes(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Assign().Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Unassign().Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil || *group.ChannelGroup.ChannelFolderID != folderID {
		t.Fatalf("channel folder ID = %v, want %d", group.ChannelGroup.ChannelFolderID, folderID)
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertChannelFolderArchived(t, ctx, base, folderID, false)
}

func TestConcurrentFolderAssignAndRemoveSerializesToRemovedFolder(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)
	created, folderID := createGroupWithUnassignedFolder(t, ctx, client, base, channelIDs, "folder assign remove race")

	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.OperationRequest(zulipmock.OperationGetSubscribers),
	)
	runStartedBeforePreviousCompletes(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Assign().Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Remove().Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID != nil {
		t.Fatalf("channel folder ID = %d, want nil after remove", *group.ChannelGroup.ChannelFolderID)
	}
	assertNoChannelFolder(t, ctx, base, channelIDs[0])
	assertNoChannelFolder(t, ctx, base, channelIDs[1])
	assertChannelFolderArchived(t, ctx, base, folderID, true)
}

func TestInitializeChannelGroupsRemovesChannelsMissingFromBotSubscriptions(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	database := newTestDatabase(t)
	client, err := channelgroup.NewClient(
		context.Background(),
		base,
		database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}

	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 2)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("stale channel").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	_, _, err = base.Unsubscribe(ctx).
		Subscriptions([]string{mockChannelName(2)}).
		Execute()
	if err != nil {
		t.Fatalf("unsubscribe bot from stale channel: %v", err)
	}

	base.FailNext(zulipmock.OperationUnsubscribe, errors.New("initialization must not unsubscribe users"))
	client, err = channelgroup.NewClient(ctx, base, database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient (re-init) error = %v", err)
	}

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[0]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("stale channel subscribers = %v, want %v", got, want)
	}
}

func TestInitializeChannelGroupsRemovesGroupWhenBackingUserGroupMissing(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	database := newTestDatabase(t)
	client, err := channelgroup.NewClient(
		context.Background(),
		base,
		database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient error = %v", err)
	}
	channelIDs := createMockBotSubscribedChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("missing user group").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	base.DeleteUserGroupForTest(created.ChannelGroupID)
	base.FailNext(zulipmock.OperationUnsubscribe, errors.New("initialization must not unsubscribe users"))
	client, err = channelgroup.NewClient(ctx, base, database,
		channelgroup.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("NewClient (re-init) error = %v", err)
	}

	_, _, err = client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err == nil {
		t.Fatalf("GetChannelGroup error = nil, want missing channel group")
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{0, 101}; !equalInt64s(got, want) {
		t.Fatalf("channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentSubscribeAndAddChannelMaterializesSubscriberOnNewChannel(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("subscribe while adding channel").
		ChannelIDs(channelIDs[:1]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	const (
		addOrigin = "UpdateChannelGroupChannels"
		subOrigin = "SubscribeToChannelGroup"
	)
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(addOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]).From(addOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(2)}, []int64{101}).
			From(addOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(addOrigin),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]).From(subOrigin),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationSubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{202},
		).From(subOrigin),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Add([]int64{channelIDs[1]}).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[0], channelIDs[1]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs = %v, want %v", got, want)
	}
	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101, 202}; !equalInt64s(got, want) {
		t.Fatalf("subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("new channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentUnsubscribeAndAddChannelDoesNotReintroduceRemovedSubscriber(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("unsubscribe while adding channel").
		ChannelIDs(channelIDs[:1]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.OperationRequest(zulipmock.OperationSubscribe),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Add([]int64{channelIDs[1]}).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
	)

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("new channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentSubscribeAndDeleteChannelDoesNotLeaveSubscriberOnRemovedChannel(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("subscribe while deleting channel").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationSubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{202},
		),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Delete([]int64{channelIDs[1]}).
				Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[0]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("removed channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentSubscribeAndUnsubscribeSameUserDeleteWins(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 1)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("subscribe then unsubscribe same user").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	const (
		subOrigin   = "SubscribeToChannelGroup"
		unsubOrigin = "UnsubscribeFromChannelGroup"
	)
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(unsubOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}).
			From(unsubOrigin),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers).From(unsubOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(subOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}).
			From(subOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(subOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}).
			From(subOrigin),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
	)

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentUnsubscribeAndSubscribeSameUserAddWins(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 1)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("unsubscribe then subscribe same user").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
	)

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101, 202}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentSerializedSameUserSubscriptionState(t *testing.T) {
	tests := []struct {
		name        string
		initiallyIn bool
		operations  []zulipmock.Operation
		wantMembers []int64
	}{
		{
			name:        "subscribe unsubscribe from absent",
			initiallyIn: false,
			operations:  []zulipmock.Operation{zulipmock.OperationSubscribe, zulipmock.OperationUnsubscribe},
			wantMembers: []int64{101},
		},
		{
			name:        "subscribe unsubscribe from present",
			initiallyIn: true,
			operations:  []zulipmock.Operation{zulipmock.OperationSubscribe, zulipmock.OperationUnsubscribe},
			wantMembers: []int64{101},
		},
		{
			name:        "unsubscribe subscribe unsubscribe subscribe unsubscribe",
			initiallyIn: true,
			operations: []zulipmock.Operation{
				zulipmock.OperationUnsubscribe,
				zulipmock.OperationSubscribe,
				zulipmock.OperationUnsubscribe,
				zulipmock.OperationSubscribe,
				zulipmock.OperationUnsubscribe,
			},
			wantMembers: []int64{101},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupCtx := context.Background()
			base := zulipmock.NewClient()
			client := newTestClient(t, base)
			channelIDs := createMockChannels(t, setupCtx, base, 1)
			initialSubscribers := []int64{101}
			if tt.initiallyIn {
				initialSubscribers = append(initialSubscribers, 202)
			}

			created, _, err := client.CreateChannelGroup(setupCtx).
				Name(tt.name).
				ChannelIDs(channelIDs).
				InitialSubscribers(zulip.UserIDsAsPrincipals(initialSubscribers...)).
				Execute()
			if err != nil {
				t.Fatalf("CreateChannelGroup error = %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			steps := sameUserSubscriptionToggleSteps(channelIDs[0], mockChannelName(1), 202, tt.operations)
			serialization := base.SerializeRequestSteps(steps...)
			defer base.ClearRequestSerialization()

			ops := make([]func() error, 0, len(tt.operations))
			startAfterSteps := make([]int, 0, len(tt.operations)-1)
			stepsBeforeOperation := 0
			for i, operation := range tt.operations {
				if i > 0 {
					startAfterSteps = append(startAfterSteps, stepsBeforeOperation)
				}

				ops = append(ops, func() error {
					//nolint:exhaustive // test table only exercises subscribe and unsubscribe operations
					switch operation {
					case zulipmock.OperationSubscribe:
						_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
							Principals(zulip.UserIDsAsPrincipals(202)).
							Execute()
						return err
					case zulipmock.OperationUnsubscribe:
						_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
							Principals(zulip.UserIDsAsPrincipals(202)).
							Execute()
						return err
					default:
						return fmt.Errorf("unsupported operation %q", operation)
					}
				})
				stepsBeforeOperation += sameUserSubscriptionToggleStepCount(operation)
			}

			runSerializedOperations(t, ctx, serialization, startAfterSteps, ops...)

			subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
			if err != nil {
				t.Fatalf("GetChannelGroupSubscribers error = %v", err)
			}
			if got := subscribers.SubscriberIDs; !equalInt64s(got, tt.wantMembers) {
				t.Fatalf("channel group subscribers = %v, want %v", got, tt.wantMembers)
			}
			wantChannelSubscribers := append([]int64{}, tt.wantMembers...)
			wantChannelSubscribers = append(wantChannelSubscribers, mockBootstrapUserID)
			if got := channelSubscribers(t, ctx, base, channelIDs[0]); !equalInt64s(got, wantChannelSubscribers) {
				t.Fatalf("channel subscribers = %v, want %v", got, wantChannelSubscribers)
			}
		})
	}
}

func TestConcurrentAddAndDeleteChannelsKeepIndependentChanges(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 3)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("add and delete channels").
		ChannelIDs(channelIDs[:2]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[2]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(3)}, []int64{101}),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Add([]int64{channelIDs[2]}).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Delete([]int64{channelIDs[0]}).
				Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[1], channelIDs[2]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("removed channel subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[2]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("added channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentTwoSubscribersAndAddChannelMaterializesBothUsers(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("two subscribers while adding channel").
		ChannelIDs(channelIDs[:1]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const (
		addOrigin = "UpdateChannelGroupChannels"
		subOrigin = "SubscribeToChannelGroup"
	)
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(addOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]).From(addOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(2)}, []int64{101}).
			From(addOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(addOrigin),
		zulipmock.UpdateUserGroupMembersRequest(created.ChannelGroupID, []int64{202}, nil).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]).From(subOrigin),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationSubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{202},
		).From(subOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(subOrigin),
		zulipmock.UpdateUserGroupMembersRequest(created.ChannelGroupID, []int64{303}, nil).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(subOrigin),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]).From(subOrigin),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationSubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{303},
		).
			From(subOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(subOrigin),
	)
	defer base.ClearRequestSerialization()

	runSerializedOperations(t, ctx, serialization, []int{1, 4},
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Add([]int64{channelIDs[1]}).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(303)).
				Execute()
			return err
		},
	)

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101, 202, 303}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, 202, 303, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("added channel subscribers = %v, want %v", got, want)
	}
}

func TestUpdateChannelGroupChannelsDoesNotCommitWhenChannelSubscribeFails(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 2)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("failing channel update").
		ChannelIDs(channelIDs[:1]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	base.FailNext(zulipmock.OperationSubscribe, errors.New("subscribe failed"))
	_, _, err = client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
		Add([]int64{channelIDs[1]}).
		Execute()
	if err == nil {
		t.Fatalf("UpdateChannelGroupChannels error = nil, want failure")
	}

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[0]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs after failed update = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("new channel subscribers after failed update = %v, want %v", got, want)
	}
}

func TestUpdateChannelGroupChannelsDoesNotResubscribeManualChannelUnsubscribe(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 3)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("preserve manual unsubscribe on channel update").
		ChannelIDs(channelIDs[:2]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	if _, _, err = base.Unsubscribe(ctx).
		Subscriptions([]string{mockChannelName(1)}).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute(); err != nil {
		t.Fatalf("manual unsubscribe from channel: %v", err)
	}

	_, _, err = client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
		Add([]int64{channelIDs[2]}).
		Execute()
	if err != nil {
		t.Fatalf("UpdateChannelGroupChannels error = %v", err)
	}

	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("manually unsubscribed channel subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[2]), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("new channel subscribers = %v, want %v", got, want)
	}
}

func TestSubscribeToChannelGroupDoesNotResubscribeManualChannelUnsubscribe(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("preserve manual unsubscribe on subscribe").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	if _, _, err = base.Unsubscribe(ctx).
		Subscriptions([]string{mockChannelName(1)}).
		Principals(zulip.UserIDsAsPrincipals(101)).
		Execute(); err != nil {
		t.Fatalf("manual unsubscribe from channel: %v", err)
	}

	_, _, err = client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute()
	if err != nil {
		t.Fatalf("SubscribeToChannelGroup error = %v", err)
	}

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101, 202}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("channel subscribers = %v, want %v", got, want)
	}
}

func TestSubscribeToChannelGroupRollsBackChannelsWhenUserGroupUpdateFails(t *testing.T) {
	assertSubscribeToChannelGroupRollback(
		t,
		"failing subscribe",
		zulipmock.OperationUpdateUserGroupMembers,
		"user group update failed",
	)
}

func TestSubscribeToChannelGroupRollsBackUserGroupWhenChannelSubscribeFails(t *testing.T) {
	assertSubscribeToChannelGroupRollback(
		t,
		"failing channel subscribe",
		zulipmock.OperationSubscribe,
		"channel subscribe failed",
	)
}

func assertSubscribeToChannelGroupRollback(
	t *testing.T,
	name string,
	failingOperation zulipmock.Operation,
	failureMessage string,
) {
	t.Helper()

	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name(name).
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	base.FailNext(failingOperation, errors.New(failureMessage))
	_, _, err = client.SubscribeToChannelGroup(ctx, created.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute()
	if err == nil {
		t.Fatalf("SubscribeToChannelGroup error = nil, want failure")
	}

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers after failed subscribe = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("channel subscribers after failed subscribe = %v, want %v", got, want)
	}
}

func TestUnsubscribeFromChannelGroupRollsBackChannelsWhenUserGroupUpdateFails(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("failing unsubscribe").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	base.FailNext(zulipmock.OperationUpdateUserGroupMembers, errors.New("user group update failed"))
	_, _, err = client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute()
	if err == nil {
		t.Fatalf("UnsubscribeFromChannelGroup error = nil, want failure")
	}

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101, 202}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers after failed unsubscribe = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("channel subscribers after failed unsubscribe = %v, want %v", got, want)
	}
}

func TestConcurrentUnsubscribeAndDeleteSameChannelLeavesNoSubscribers(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 1)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("unsubscribe while deleting only channel").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	const (
		unsubOrigin  = "UnsubscribeFromChannelGroup"
		deleteOrigin = "UpdateChannelGroupChannels"
	)
	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]).From(unsubOrigin),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers).From(deleteOrigin),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}).
			From(unsubOrigin),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers).From(unsubOrigin),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err = client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err = client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Delete([]int64{channelIDs[0]}).
				Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got := group.ChannelGroup.ChannelIDs; len(got) != 0 {
		t.Fatalf("channel IDs = %v, want empty", got)
	}
	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("removed channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentUnsubscribeThenAddChannelDoesNotReintroduceRemovedSubscriber(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("unsubscribe before adding channel").
		ChannelIDs(channelIDs[:1]).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(2)}, []int64{101}),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Add([]int64{channelIDs[1]}).
				Execute()
			return err
		},
	)

	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("channel group subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("new channel subscribers = %v, want %v", got, want)
	}
}

func TestConcurrentUnsubscribeAndDeleteDifferentChannelLeavesRemainingChannelsIntact(t *testing.T) {
	setupCtx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, setupCtx, base, 2)

	created, _, err := client.CreateChannelGroup(setupCtx).
		Name("unsubscribe while deleting different channel").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101, 202)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationUnsubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{202},
		),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
	)
	defer base.ClearRequestSerialization()

	runSerializedPair(t, ctx, serialization,
		func() error {
			_, _, err := client.UnsubscribeFromChannelGroup(ctx, created.ChannelGroupID).
				Principals(zulip.UserIDsAsPrincipals(202)).
				Execute()
			return err
		},
		func() error {
			_, _, err := client.UpdateChannelGroupChannels(ctx, created.ChannelGroupID).
				Delete([]int64{channelIDs[1]}).
				Execute()
			return err
		},
	)

	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if got, want := group.ChannelGroup.ChannelIDs, []int64{channelIDs[0]}; !equalInt64s(got, want) {
		t.Fatalf("channel IDs = %v, want %v", got, want)
	}
	subscribers, _, err := client.GetChannelGroupSubscribers(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroupSubscribers error = %v", err)
	}
	if got, want := subscribers.SubscriberIDs, []int64{101}; !equalInt64s(got, want) {
		t.Fatalf("subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[0]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("remaining channel subscribers = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, channelIDs[1]), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Fatalf("removed channel subscribers = %v, want %v", got, want)
	}
}

const mockBootstrapUserID int64 = 9000

func runSerializedPair(
	t *testing.T,
	ctx context.Context,
	serialization *zulipmock.RequestSerialization,
	first func() error,
	second func() error,
) {
	t.Helper()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- first()
	}()
	if err := serialization.WaitForSteps(ctx, 1); err != nil {
		t.Fatalf("first serialized request was not observed: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- second()
	}()

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
	if err := serialization.Wait(ctx); err != nil {
		t.Fatalf("serialization did not observe all requests: %v", err)
	}
}

func runStartedBeforePreviousCompletes(
	t *testing.T,
	ctx context.Context,
	serialization *zulipmock.RequestSerialization,
	first func() error,
	second func() error,
) {
	t.Helper()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- first()
	}()
	if err := serialization.WaitForSteps(ctx, 1); err != nil {
		t.Fatalf("first serialized request was not observed: %v", err)
	}

	wg.Add(1)
	secondStarted := make(chan struct{})
	go func() {
		defer wg.Done()
		close(secondStarted)
		errs <- second()
	}()
	<-secondStarted
	serialization.Close()

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
}

func runSerializedOperations(
	t *testing.T,
	ctx context.Context,
	serialization *zulipmock.RequestSerialization,
	startAfterSteps []int,
	ops ...func() error,
) {
	t.Helper()
	if len(ops) == 0 {
		return
	}
	if len(startAfterSteps) != len(ops)-1 {
		t.Fatalf("startAfterSteps length = %d, want %d", len(startAfterSteps), len(ops)-1)
	}

	errs := make(chan error, len(ops))
	var wg sync.WaitGroup
	start := func(op func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- op()
		}()
	}

	start(ops[0])
	for i, steps := range startAfterSteps {
		if err := serialization.WaitForSteps(ctx, steps); err != nil {
			t.Fatalf("serialized request %d was not observed: %v", steps, err)
		}
		start(ops[i+1])
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
	if err := serialization.Wait(ctx); err != nil {
		t.Fatalf("serialization did not observe all requests: %v", err)
	}
}

func sameUserSubscriptionToggleSteps(
	channelID int64,
	channelName string,
	userID int64,
	operations []zulipmock.Operation,
) []zulipmock.RequestStep {
	steps := make([]zulipmock.RequestStep, 0, len(operations)*6)
	for _, operation := range operations {
		//nolint:exhaustive // serialization steps are only defined for subscribe/unsubscribe toggles
		switch operation {
		case zulipmock.OperationSubscribe:
			steps = append(steps,
				zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
				zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelID),
				zulipmock.OperationRequest(zulipmock.OperationSubscribe),
			)
		case zulipmock.OperationUnsubscribe:
			steps = append(steps,
				zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelID),
				zulipmock.SubscriptionRequest(operation, []string{channelName}, []int64{userID}),
				zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
			)
		}
	}
	return steps
}

func sameUserSubscriptionToggleStepCount(operation zulipmock.Operation) int {
	//nolint:exhaustive // unsupported operations use the default branch
	switch operation {
	case zulipmock.OperationSubscribe:
		return 3
	case zulipmock.OperationUnsubscribe:
		return 3
	default:
		return 0
	}
}

func mockChannelName(index int) string {
	return fmt.Sprintf("course-%d", index)
}

func createMockChannels(t *testing.T, ctx context.Context, client zulipmock.Client, count int) []int64 {
	t.Helper()

	ids := make([]int64, 0, count)
	for i := range count {
		name := mockChannelName(i + 1)
		_, _, err := client.Subscribe(ctx).
			Subscriptions([]channels.SubscriptionRequest{{Name: name}}).
			Principals(zulip.UserIDsAsPrincipals(mockBootstrapUserID)).
			Execute()
		if err != nil {
			t.Fatalf("create mock channel %q: %v", name, err)
		}
		ids = append(ids, int64(i+1))
	}
	return ids
}

func createMockBotSubscribedChannels(t *testing.T, ctx context.Context, client zulipmock.Client, count int) []int64 {
	t.Helper()

	ids := make([]int64, 0, count)
	for i := range count {
		name := mockChannelName(i + 1)
		_, _, err := client.Subscribe(ctx).
			Subscriptions([]channels.SubscriptionRequest{{Name: name}}).
			Execute()
		if err != nil {
			t.Fatalf("create mock bot-subscribed channel %q: %v", name, err)
		}
		ids = append(ids, int64(i+1))
	}
	return ids
}

func createGroupWithUnassignedFolder(
	t *testing.T,
	ctx context.Context,
	client channelgroup.Client,
	base zulipmock.Client,
	channelIDs []int64,
	name string,
) (*channelgroup.CreateChannelGroupResponse, int64) {
	t.Helper()

	created, _, err := client.CreateChannelGroup(ctx).
		Name(name).
		ChannelIDs(channelIDs).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}
	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Add().Execute(); err != nil {
		t.Fatalf("Add folder error = %v", err)
	}
	group, _, err := client.GetChannelGroup(ctx, created.ChannelGroupID).Execute()
	if err != nil {
		t.Fatalf("GetChannelGroup error = %v", err)
	}
	if group.ChannelGroup.ChannelFolderID == nil {
		t.Fatal("channel folder ID = nil after add")
	}
	folderID := *group.ChannelGroup.ChannelFolderID
	if _, _, err = client.UpdateChannelGroupFolder(ctx, created.ChannelGroupID).Unassign().Execute(); err != nil {
		t.Fatalf("Unassign folder error = %v", err)
	}
	for _, channelID := range channelIDs {
		assertNoChannelFolder(t, ctx, base, channelID)
	}
	return created, folderID
}

func assertChannelFolderID(
	t *testing.T,
	ctx context.Context,
	client zulipmock.Client,
	channelID int64,
	wantFolderID int64,
) {
	t.Helper()

	resp, _, err := client.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID(%d) error = %v", channelID, err)
	}
	if resp.Channel.FolderID == nil {
		t.Fatalf("channel %d folder ID = nil, want %d", channelID, wantFolderID)
	}
	if *resp.Channel.FolderID != wantFolderID {
		t.Fatalf("channel %d folder ID = %d, want %d", channelID, *resp.Channel.FolderID, wantFolderID)
	}
}

func assertChannelFolderArchived(
	t *testing.T,
	ctx context.Context,
	client zulipmock.Client,
	folderID int64,
	wantArchived bool,
) {
	t.Helper()

	resp, _, err := client.GetChannelFolders(ctx).Execute()
	if err != nil {
		t.Fatalf("GetChannelFolders error = %v", err)
	}
	for _, folder := range resp.ChannelFolders {
		if folder.ID != folderID {
			continue
		}
		if folder.IsArchived != wantArchived {
			t.Fatalf("folder %d archived = %t, want %t", folderID, folder.IsArchived, wantArchived)
		}
		return
	}
	t.Fatalf("folder %d not found in %+v", folderID, resp.ChannelFolders)
}

func assertOnlyChannelFolderArchived(
	t *testing.T,
	ctx context.Context,
	client zulipmock.Client,
	wantArchived bool,
) {
	t.Helper()

	resp, _, err := client.GetChannelFolders(ctx).Execute()
	if err != nil {
		t.Fatalf("GetChannelFolders error = %v", err)
	}
	if len(resp.ChannelFolders) != 1 {
		t.Fatalf("channel folders = %d, want 1", len(resp.ChannelFolders))
	}
	if resp.ChannelFolders[0].IsArchived != wantArchived {
		t.Fatalf("folder archived = %t, want %t", resp.ChannelFolders[0].IsArchived, wantArchived)
	}
}

func assertNoChannelFolder(t *testing.T, ctx context.Context, client zulipmock.Client, channelID int64) {
	t.Helper()

	resp, _, err := client.GetChannelByID(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID(%d) error = %v", channelID, err)
	}
	if resp.Channel.FolderID != nil {
		t.Fatalf("channel %d folder ID = %d, want nil", channelID, *resp.Channel.FolderID)
	}
}

func channelSubscribers(t *testing.T, ctx context.Context, client zulipmock.Client, channelID int64) []int64 {
	t.Helper()

	resp, _, err := client.GetSubscribers(ctx, channelID).Execute()
	if err != nil {
		t.Fatalf("GetSubscribers(%d) error = %v", channelID, err)
	}
	return resp.Subscribers
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func findUserGroupByID(groups []zulip.UserGroup, id int64) (zulip.UserGroup, bool) {
	for _, group := range groups {
		if group.ID == id {
			return group, true
		}
	}
	return zulip.UserGroup{}, false
}

func findUserGroupByName(groups []zulip.UserGroup, name string) (zulip.UserGroup, bool) {
	for _, group := range groups {
		if group.Name == name {
			return group, true
		}
	}
	return zulip.UserGroup{}, false
}

func assertAdminGroupSettingValue(t *testing.T, name string, got zulip.GroupSettingValue) {
	t.Helper()
	if got.GroupID == nil {
		t.Fatalf("%s.GroupID = nil, want administrators system group", name)
	}
	if *got.GroupID != 42 {
		t.Fatalf("%s.GroupID = %d, want administrators system group ID 42", name, *got.GroupID)
	}
}

// TestUnsubscribeFromChannelGroupKeepsSharedChannelWhenUserStillInOtherGroup verifies
// that a user is only unsubscribed from a channel when none of the channel groups they
// belong to still contain that channel. If a channel is shared between two groups and the
// user unsubscribes from only one, they must remain in the channel.
func TestUnsubscribeFromChannelGroupKeepsSharedChannelWhenUserStillInOtherGroup(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	// Channel 1 is shared between group A and group B.
	// Channel 2 is exclusive to group B.
	channelIDs := createMockChannels(t, ctx, base, 2)
	sharedChannelID := channelIDs[0]
	exclusiveChannelID := channelIDs[1]

	groupA, _, err := client.CreateChannelGroup(ctx).
		Name("group-a").
		ChannelIDs([]int64{sharedChannelID}).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup(A) error = %v", err)
	}

	groupB, _, err := client.CreateChannelGroup(ctx).
		Name("group-b").
		ChannelIDs([]int64{sharedChannelID, exclusiveChannelID}).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup(B) error = %v", err)
	}

	// Subscribe user 202 to both groups.
	if _, _, err = client.SubscribeToChannelGroup(ctx, groupA.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute(); err != nil {
		t.Fatalf("SubscribeToChannelGroup(A, 202) error = %v", err)
	}
	if _, _, err = client.SubscribeToChannelGroup(ctx, groupB.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute(); err != nil {
		t.Fatalf("SubscribeToChannelGroup(B, 202) error = %v", err)
	}

	// Unsubscribe user 202 from group A only.
	if _, _, err = client.UnsubscribeFromChannelGroup(ctx, groupA.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute(); err != nil {
		t.Fatalf("UnsubscribeFromChannelGroup(A, 202) error = %v", err)
	}

	// User 202 must no longer be a member of group A.
	subA, _, err := client.GetIsChannelGroupSubscriber(ctx, groupA.ChannelGroupID, 202).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber(A, 202) error = %v", err)
	}
	if subA.IsSubscriber {
		t.Error("expected user 202 to be removed from group A")
	}

	// User 202 must still be a member of group B.
	subB, _, err := client.GetIsChannelGroupSubscriber(ctx, groupB.ChannelGroupID, 202).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber(B, 202) error = %v", err)
	}
	if !subB.IsSubscriber {
		t.Error("expected user 202 to remain in group B")
	}

	// User 202 must still be subscribed to the shared channel because they are still in group B.
	if got, want := channelSubscribers(t, ctx, base, sharedChannelID), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Errorf(
			"shared channel subscribers after unsubscribe from A = %v, want %v (user 202 still in group B)",
			got,
			want,
		)
	}

	// User 202 must still be subscribed to the exclusive group B channel.
	if got, want := channelSubscribers(t, ctx, base, exclusiveChannelID), []int64{101, 202, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Errorf("exclusive channel B subscribers after unsubscribe from A = %v, want %v", got, want)
	}

	// After also unsubscribing from group B, user 202 must be removed from both channels.
	if _, _, err = client.UnsubscribeFromChannelGroup(ctx, groupB.ChannelGroupID).
		Principals(zulip.UserIDsAsPrincipals(202)).
		Execute(); err != nil {
		t.Fatalf("UnsubscribeFromChannelGroup(B, 202) error = %v", err)
	}

	if got, want := channelSubscribers(t, ctx, base, sharedChannelID), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Errorf("shared channel subscribers after full unsubscribe = %v, want %v", got, want)
	}
	if got, want := channelSubscribers(t, ctx, base, exclusiveChannelID), []int64{101, mockBootstrapUserID}; !equalInt64s(
		got,
		want,
	) {
		t.Errorf("exclusive channel B subscribers after full unsubscribe = %v, want %v", got, want)
	}
}
