package zulipmock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	"github.com/tum-zulip/go-zulip/zulip/client"
)

func TestClientImplementsUpstreamClient(t *testing.T) {
	var _ client.Client = NewClient()
}

func TestBuilderExecuteReturnsNil(t *testing.T) {
	resp, httpResp, err := NewClient().SendMessage(context.Background()).Execute()
	if resp != nil {
		t.Fatalf("response = %#v, want nil", resp)
	}
	if httpResp != nil {
		t.Fatalf("http response = %#v, want nil", httpResp)
	}
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
}

func TestUserGroupsAreInMemoryPerClient(t *testing.T) {
	ctx := context.Background()
	client := NewClient()

	created, _, err := client.CreateUserGroup(ctx).
		Name("testers").
		Description("Test users").
		Members([]int64{2, 1}).
		Execute()
	if err != nil {
		t.Fatalf("CreateUserGroup error = %v", err)
	}

	members, _, err := client.GetUserGroupMembers(ctx, created.GroupID).Execute()
	if err != nil {
		t.Fatalf("GetUserGroupMembers error = %v", err)
	}
	if got, want := members.Members, []int64{1, 2}; !equalInt64s(got, want) {
		t.Fatalf("members = %v, want %v", got, want)
	}

	_, _, err = client.UpdateUserGroupMembers(ctx, created.GroupID).
		Delete([]int64{1}).
		Add([]int64{3}).
		Execute()
	if err != nil {
		t.Fatalf("UpdateUserGroupMembers error = %v", err)
	}

	status, _, err := client.GetIsUserGroupMember(ctx, created.GroupID, 3).Execute()
	if err != nil {
		t.Fatalf("GetIsUserGroupMember error = %v", err)
	}
	if !status.IsUserGroupMember {
		t.Fatalf("IsUserGroupMember = false, want true")
	}

	members, _, err = client.GetUserGroupMembers(ctx, created.GroupID).Execute()
	if err != nil {
		t.Fatalf("GetUserGroupMembers after update error = %v", err)
	}
	if got, want := members.Members, []int64{2, 3}; !equalInt64s(got, want) {
		t.Fatalf("members after update = %v, want %v", got, want)
	}

	_, _, err = client.DeactivateUserGroup(ctx, created.GroupID).Execute()
	if err != nil {
		t.Fatalf("DeactivateUserGroup error = %v", err)
	}
	_, _, err = client.GetUserGroupMembers(ctx, created.GroupID).Execute()
	if err != nil {
		t.Fatalf("GetUserGroupMembers after deactivate error = %v", err)
	}

	otherClient := NewClient()
	_, _, err = otherClient.GetUserGroupMembers(ctx, created.GroupID).Execute()
	if err == nil {
		t.Fatalf("second client found first client's user group")
	}
}

func TestFailNextFailsOneMatchingRequest(t *testing.T) {
	ctx := context.Background()
	client := NewClient()
	injected := errors.New("injected subscribe failure")

	client.FailNext(OperationSubscribe, injected)
	_, _, err := client.Subscribe(ctx).
		Subscriptions([]channels.SubscriptionRequest{{Name: "course"}}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if !errors.Is(err, injected) {
		t.Fatalf("Subscribe error = %v, want %v", err, injected)
	}

	resp, _, err := client.Subscribe(ctx).
		Subscriptions([]channels.SubscriptionRequest{{Name: "course"}}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("second Subscribe error = %v", err)
	}
	if got, want := resp.Subscribed["10"], []string{"course"}; !equalStrings(got, want) {
		t.Fatalf("subscribed[10] = %v, want %v", got, want)
	}
}

func TestSerializeRequestsForcesOperationOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	client := NewClient()
	_, _, err := client.Subscribe(ctx).
		Subscriptions([]channels.SubscriptionRequest{{Name: "course"}}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("setup Subscribe error = %v", err)
	}

	serialization := client.SerializeRequests(OperationGetSubscribers, OperationSubscribe)
	defer client.ClearRequestSerialization()

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, err := client.Subscribe(ctx).
			Subscriptions([]channels.SubscriptionRequest{{Name: "course"}}).
			Principals(zulip.UserIDsAsPrincipals(20)).
			Execute()
		errs <- err
	}()

	subscribers, _, err := client.GetSubscribers(ctx, 1).Execute()
	if err != nil {
		t.Fatalf("GetSubscribers error = %v", err)
	}
	if got, want := subscribers.Subscribers, []int64{10}; !equalInt64s(got, want) {
		t.Fatalf("subscribers before serialized Subscribe = %v, want %v", got, want)
	}
	if err := serialization.Wait(ctx); err != nil {
		t.Fatalf("serialization did not observe all requests: %v", err)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("serialized Subscribe error = %v", err)
		}
	}

	subscribers, _, err = client.GetSubscribers(ctx, 1).Execute()
	if err != nil {
		t.Fatalf("final GetSubscribers error = %v", err)
	}
	if got, want := subscribers.Subscribers, []int64{10, 20}; !equalInt64s(got, want) {
		t.Fatalf("final subscribers = %v, want %v", got, want)
	}
}

func TestSubscribeUnsubscribeAndGetChannelByID(t *testing.T) {
	ctx := context.Background()
	client := NewClient()
	description := "Course channel"

	resp, _, err := client.Subscribe(ctx).
		Subscriptions([]channels.SubscriptionRequest{{Name: "course", Description: &description}}).
		Principals(zulip.UserIDsAsPrincipals(10, 20)).
		Execute()
	if err != nil {
		t.Fatalf("Subscribe error = %v", err)
	}
	if got, want := resp.Subscribed["10"], []string{"course"}; !equalStrings(got, want) {
		t.Fatalf("subscribed[10] = %v, want %v", got, want)
	}

	resp, _, err = client.Subscribe(ctx).
		Subscriptions([]channels.SubscriptionRequest{{Name: "course"}}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("second Subscribe error = %v", err)
	}
	if got, want := resp.AlreadySubscribed["10"], []string{"course"}; !equalStrings(got, want) {
		t.Fatalf("already_subscribed[10] = %v, want %v", got, want)
	}

	channel, _, err := client.GetChannelByID(ctx, 1).Execute()
	if err != nil {
		t.Fatalf("GetChannelByID error = %v", err)
	}
	if channel.Channel.Name != "course" || channel.Channel.Description != description {
		t.Fatalf("channel = %#v, want name %q and description %q", channel.Channel, "course", description)
	}

	unsubscribed, _, err := client.Unsubscribe(ctx).
		Subscriptions([]string{"course"}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("Unsubscribe error = %v", err)
	}
	if got, want := unsubscribed.Removed, []string{"course"}; !equalStrings(got, want) {
		t.Fatalf("removed = %v, want %v", got, want)
	}

	unsubscribed, _, err = client.Unsubscribe(ctx).
		Subscriptions([]string{"course"}).
		Principals(zulip.UserIDsAsPrincipals(10)).
		Execute()
	if err != nil {
		t.Fatalf("second Unsubscribe error = %v", err)
	}
	if got, want := unsubscribed.NotRemoved, []string{"course"}; !equalStrings(got, want) {
		t.Fatalf("not_removed = %v, want %v", got, want)
	}
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

func equalStrings(a, b []string) bool {
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
