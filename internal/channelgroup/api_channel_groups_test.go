package channelgroup

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

	schema, err := os.ReadFile("db/schema.sql")
	if err != nil {
		t.Fatalf("read channelgroup schema: %v", err)
	}
	if _, err = database.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply channelgroup schema: %v", err)
	}
	return database
}

func newTestClient(t *testing.T, base zulipmock.Client) Client {
	t.Helper()

	database := newTestDatabase(t)
	return NewClient(base, database, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func newInitializedTestClient(t *testing.T, ctx context.Context, base zulipmock.Client, database *sql.DB) Client {
	t.Helper()

	client, err := NewInitializedClient(ctx, base, database, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("NewInitializedClient error = %v", err)
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

func TestCreateChannelGroupCreatesRestrictiveUserGroup(t *testing.T) {
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
	if len(groups.UserGroups) != 1 {
		t.Fatalf("created user groups = %d, want 1", len(groups.UserGroups))
	}

	group := groups.UserGroups[0]
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

func TestInitializeChannelGroupsRemovesChannelsMissingFromBotSubscriptions(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	database := newTestDatabase(t)
	client := NewClient(base, database, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	client = newInitializedTestClient(t, ctx, base, database)

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
	client := NewClient(base, database, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
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
	client = newInitializedTestClient(t, ctx, base, database)

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
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}),
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
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.SubscriptionRequest(
			zulipmock.OperationSubscribe,
			[]string{mockChannelName(1), mockChannelName(2)},
			[]int64{202},
		),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
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
	serialization := base.SerializeRequestSteps(
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationUnsubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
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
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}),
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

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	serialization := base.SerializeRequestSteps(
		zulipmock.OperationRequest(zulipmock.OperationGetUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{202}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[0]),
		zulipmock.SubscriptionRequest(zulipmock.OperationSubscribe, []string{mockChannelName(1)}, []int64{303}),
		zulipmock.OperationRequest(zulipmock.OperationUpdateUserGroupMembers),
		zulipmock.ChannelRequest(zulipmock.OperationGetChannelByID, channelIDs[1]),
		zulipmock.OperationRequest(zulipmock.OperationSubscribe),
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

func TestSubscribeToChannelGroupRollsBackChannelsWhenUserGroupUpdateFails(t *testing.T) {
	ctx := context.Background()
	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	channelIDs := createMockChannels(t, ctx, base, 1)

	created, _, err := client.CreateChannelGroup(ctx).
		Name("failing subscribe").
		ChannelIDs(channelIDs).
		InitialSubscribers(zulip.UserIDsAsPrincipals(101)).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup error = %v", err)
	}

	base.FailNext(zulipmock.OperationUpdateUserGroupMembers, errors.New("user group update failed"))
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

func mockChannelName(index int) string {
	return fmt.Sprintf("course-%d", index)
}

func createMockChannels(t *testing.T, ctx context.Context, client zulipmock.Client, count int) []int64 {
	t.Helper()

	ids := make([]int64, 0, count)
	for i := 0; i < count; i++ {
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

func assertAdminGroupSettingValue(t *testing.T, name string, got zulip.GroupSettingValue) {
	t.Helper()
	if got.GroupID == nil {
		t.Fatalf("%s.GroupID = nil, want %d", name, zulipAdministratorsSystemGroupID)
	}
	if *got.GroupID != zulipAdministratorsSystemGroupID {
		t.Fatalf("%s.GroupID = %d, want %d", name, *got.GroupID, zulipAdministratorsSystemGroupID)
	}
}
