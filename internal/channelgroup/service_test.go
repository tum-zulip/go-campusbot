package channelgroup

import (
	"context"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
	"github.com/tum-zulip/go-zulip/zulip"
)

func TestGroupServiceChannelGroupExists(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	created, _, err := client.CreateChannelGroup(ctx).Name("test-group").Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup() failed: %v", err)
	}
	groupID := created.ChannelGroupID

	svc := NewGroupService(client)

	exists, err := svc.ChannelGroupExists(ctx, groupID)
	if err != nil {
		t.Fatalf("ChannelGroupExists(existing) error = %v", err)
	}
	if !exists {
		t.Error("expected existing channel group to be reported as existing")
	}

	exists, err = svc.ChannelGroupExists(ctx, groupID+9999)
	if err != nil {
		t.Fatalf("ChannelGroupExists(missing) error = %v, want nil", err)
	}
	if exists {
		t.Error("expected non-existent channel group to be reported as missing")
	}
}

func TestGroupServiceSubscribeUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	// Create a channel group first
	created, _, err := client.CreateChannelGroup(ctx).
		Name("test-group").
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup() failed: %v", err)
	}
	groupID := created.ChannelGroupID

	svc := NewGroupService(client)

	if err := svc.SubscribeUser(ctx, 42, groupID); err != nil {
		t.Fatalf("SubscribeUser() failed: %v", err)
	}

	// Verify user is a member
	resp, _, err := client.GetIsChannelGroupSubscriber(ctx, groupID, 42).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber() failed: %v", err)
	}
	if !resp.IsSubscriber {
		t.Error("expected user 42 to be a subscriber")
	}
}

func TestGroupServiceUnsubscribeUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	// Create group and subscribe a user
	created, _, err := client.CreateChannelGroup(ctx).
		Name("test-group").
		InitialSubscribers(zulip.Principals{UserIDs: &[]int64{99}}).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup() failed: %v", err)
	}
	groupID := created.ChannelGroupID

	svc := NewGroupService(client)

	if err := svc.UnsubscribeUser(ctx, 99, groupID); err != nil {
		t.Fatalf("UnsubscribeUser() failed: %v", err)
	}

	resp, _, err := client.GetIsChannelGroupSubscriber(ctx, groupID, 99).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber() failed: %v", err)
	}
	if resp.IsSubscriber {
		t.Error("expected user 99 to no longer be a subscriber")
	}
}

func TestGroupServiceListZulipUserGroupsReturnsSummaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()

	// Seed two user groups directly via the upstream API.
	if _, _, err := base.CreateUserGroup(ctx).
		Name("FirstGroup").
		Description("seeded-first").
		Members([]int64{1, 2, 3}).
		Execute(); err != nil {
		t.Fatalf("CreateUserGroup(first) failed: %v", err)
	}
	if _, _, err := base.CreateUserGroup(ctx).
		Name("SecondGroup").
		Description("seeded-second").
		Members([]int64{4}).
		Execute(); err != nil {
		t.Fatalf("CreateUserGroup(second) failed: %v", err)
	}

	client := newTestClient(t, base)
	svc := NewGroupService(client)
	summaries, err := svc.ListZulipUserGroups(ctx)
	if err != nil {
		t.Fatalf("ListZulipUserGroups() error = %v", err)
	}

	byName := make(map[string]ZulipUserGroupSummary, len(summaries))
	for _, s := range summaries {
		byName[s.Name] = s
	}

	first, ok := byName["FirstGroup"]
	if !ok {
		t.Fatalf("expected 'FirstGroup' in result, got %+v", summaries)
	}
	if first.MemberCount != 3 {
		t.Errorf("expected first group member count 3, got %d", first.MemberCount)
	}
	if first.Description != "seeded-first" {
		t.Errorf("expected first group description 'seeded-first', got %q", first.Description)
	}

	second, ok := byName["SecondGroup"]
	if !ok {
		t.Fatalf("expected 'SecondGroup' in result, got %+v", summaries)
	}
	if second.MemberCount != 1 {
		t.Errorf("expected second group member count 1, got %d", second.MemberCount)
	}
}

func TestGroupServiceImportZulipUserGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	// Seed a user group in Zulip so the import has something to anchor on.
	created, _, err := base.CreateUserGroup(ctx).
		Name("Importable").
		Description("seed").
		Members([]int64{10}).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup() failed: %v", err)
	}

	client := newTestClient(t, base)
	svc := NewGroupService(client)

	// Not local yet.
	exists, err := svc.ChannelGroupExists(ctx, created.GroupID)
	if err != nil {
		t.Fatalf("ChannelGroupExists() before import: %v", err)
	}
	if exists {
		t.Fatalf("expected group %d not to exist locally before import", created.GroupID)
	}

	// Import.
	if err := svc.ImportZulipUserGroup(ctx, created.GroupID); err != nil {
		t.Fatalf("ImportZulipUserGroup() failed: %v", err)
	}

	exists, err = svc.ChannelGroupExists(ctx, created.GroupID)
	if err != nil {
		t.Fatalf("ChannelGroupExists() after import: %v", err)
	}
	if !exists {
		t.Errorf("expected group %d to exist locally after import", created.GroupID)
	}

	// Idempotent.
	if err := svc.ImportZulipUserGroup(ctx, created.GroupID); err != nil {
		t.Errorf("expected idempotent ImportZulipUserGroup, got: %v", err)
	}
}

func TestGroupServiceListZulipUserGroupsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	client := newTestClient(t, base)
	svc := NewGroupService(client)

	summaries, err := svc.ListZulipUserGroups(ctx)
	if err != nil {
		t.Fatalf("ListZulipUserGroups() error = %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected no Zulip user groups, got %+v", summaries)
	}
}

func TestGroupServiceUnsubscribeUserKeepChannels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := zulipmock.NewClient()
	client := newTestClient(t, base)

	// Create group with initial subscriber
	created, _, err := client.CreateChannelGroup(ctx).
		Name("test-group").
		InitialSubscribers(zulip.Principals{UserIDs: &[]int64{77}}).
		Execute()
	if err != nil {
		t.Fatalf("CreateChannelGroup() failed: %v", err)
	}
	groupID := created.ChannelGroupID

	svc := NewGroupService(client)

	// Unsubscribe keeping channels — user should be removed from user group
	if err := svc.UnsubscribeUserKeepChannels(ctx, 77, groupID); err != nil {
		t.Fatalf("UnsubscribeUserKeepChannels() failed: %v", err)
	}

	resp, _, err := client.GetIsChannelGroupSubscriber(ctx, groupID, 77).Execute()
	if err != nil {
		t.Fatalf("GetIsChannelGroupSubscriber() failed: %v", err)
	}
	if resp.IsSubscriber {
		t.Error("expected user 77 to no longer be a subscriber of the group")
	}
}
