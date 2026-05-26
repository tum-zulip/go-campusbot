// Package channelgroup extends the upstream go-zulip client with endpoints
// for "channel groups": an application-level aggregate backed by one Zulip
// user group. Subscribing a user to a channel group subscribes them to every
// channel in the group and adds them to the backing user group.
//
// The API surface intentionally mirrors the shape of the upstream
// user-group endpoints (see zulip/api/users.APIUsers) so that callers
// already familiar with go-zulip can use it without context switching.
package channelgroup

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/tum-zulip/go-campusbot/internal/callorigin"
	channelgroupdb "github.com/tum-zulip/go-campusbot/internal/channelgroup/db"
	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	"github.com/tum-zulip/go-zulip/zulip/client"
)

// Names of the high-level channel-group operations, propagated through ctx
// via [callorigin] so test doubles can distinguish which logical op issued a
// low-level call. Unused in production.
const (
	originCreate         = "CreateChannelGroup"
	originUpdateChannels = "UpdateChannelGroupChannels"
	originSubscribe      = "SubscribeToChannelGroup"
	originUnsubscribe    = "UnsubscribeFromChannelGroup"
)

// Client is the campusbot Zulip client. It is a drop-in replacement for
// client.Client and additionally exposes channel-group endpoints.
type Client interface {
	client.Client
	APIChannelGroups
}

// channelGroupClient embeds the upstream client so every existing endpoint
// is forwarded unchanged, and composes the channel-groups service on top.
type channelGroupClient struct {
	client.Client
	APIChannelGroups
}

type ClientOption func(*channelGroups)

// WithLogger configures structured logging for channel-group operations.
func WithLogger(logger *slog.Logger) ClientOption {
	return func(s *channelGroups) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// NewClient wraps an existing upstream client.Client with channel-group
// endpoints backed by database. The wrapper does not own either lifecycle.
func NewClient(base client.Client, database *sql.DB, opts ...ClientOption) Client {
	service := newChannelGroups(base, database, opts...)
	return newClientWithService(base, service)
}

// NewInitializedClient wraps an upstream client and reconciles persisted
// channel-group metadata against Zulip state before returning it.
func NewInitializedClient(
	ctx context.Context,
	base client.Client,
	database *sql.DB,
	opts ...ClientOption,
) (Client, error) {
	service := newChannelGroups(base, database, opts...)
	if err := service.InitializeChannelGroups(ctx); err != nil {
		return nil, err
	}
	return newClientWithService(base, service), nil
}

func newClientWithService(base client.Client, service APIChannelGroups) Client {
	return &channelGroupClient{
		Client:           base,
		APIChannelGroups: service,
	}
}

type channelGroups struct {
	base    client.Client
	queries *channelgroupdb.Queries
	logger  *slog.Logger
}

func newChannelGroups(base client.Client, database *sql.DB, opts ...ClientOption) *channelGroups {
	service := &channelGroups{
		base:    base,
		queries: channelgroupdb.New(database),
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(service)
	}
	return service
}

var _ APIChannelGroups = (*channelGroups)(nil)

const zulipAdministratorsSystemGroupID int64 = 2

func (s *channelGroups) InitializeChannelGroups(ctx context.Context) error {
	groups, err := s.getGroups(ctx)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}

	userGroups, err := s.userGroupsByID(ctx)
	if err != nil {
		return err
	}
	subscribedChannelIDs, err := s.subscribedChannelIDs(ctx)
	if err != nil {
		return err
	}

	for _, group := range groups {
		userGroup, ok := userGroups[group.ID]
		if !ok || userGroup.Deactivated {
			if err = s.queries.DeleteChannelGroup(ctx, group.ID); err != nil {
				return err
			}
			s.logger.InfoContext(ctx, "removed channel group with missing or deactivated user group",
				"channel_group_id", group.ID,
				"user_group_exists", ok,
			)
			continue
		}

		for _, channelID := range group.ChannelIDs {
			if _, ok = subscribedChannelIDs[channelID]; ok {
				continue
			}
			if err = s.queries.RemoveChannelGroupChannel(ctx, channelgroupdb.RemoveChannelGroupChannelParams{
				ChannelGroupID: group.ID,
				ChannelID:      channelID,
			}); err != nil {
				return err
			}
			s.logger.InfoContext(ctx, "removed stale channel from channel group",
				"channel_group_id", group.ID,
				"channel_id", channelID,
			)
		}
	}

	return nil
}

func (s *channelGroups) CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest {
	return CreateChannelGroupRequest{ctx: ctx, apiService: s}
}

func (s *channelGroups) CreateChannelGroupExecute(
	r CreateChannelGroupRequest,
) (*CreateChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originCreate)
	if r.name == nil {
		return nil, nil, errors.New("name is required and must be specified")
	}

	initialMembers, err := userIDPrincipals(r.initialSubscribers)
	if err != nil {
		return nil, nil, err
	}

	group := ChannelGroup{
		Name:       *r.name,
		ChannelIDs: nil,
	}
	if r.channelIDs != nil {
		group.ChannelIDs = uniqueInt64s(*r.channelIDs)
	}

	userGroupResp, _, err := s.base.CreateUserGroup(r.ctx).
		Name(group.Name).
		Description("").
		Members(initialMembers).
		CanAddMembersGroup(administratorsOnlyGroupSetting()).
		CanJoinGroup(administratorsOnlyGroupSetting()).
		CanLeaveGroup(administratorsOnlyGroupSetting()).
		CanManageGroup(administratorsOnlyGroupSetting()).
		CanMentionGroup(administratorsOnlyGroupSetting()).
		CanRemoveMembersGroup(administratorsOnlyGroupSetting()).
		Execute()
	if err != nil {
		return nil, nil, err
	}
	group.ID = userGroupResp.GroupID

	if len(group.ChannelIDs) > 0 && len(initialMembers) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, initialMembers); err != nil {
			return nil, nil, err
		}
	}

	dbGroupID, err := s.queries.CreateChannelGroup(r.ctx, group.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, channelID := range group.ChannelIDs {
		if err = s.queries.AddChannelGroupChannel(r.ctx, channelgroupdb.AddChannelGroupChannelParams{
			ChannelGroupID: dbGroupID,
			ChannelID:      channelID,
		}); err != nil {
			return nil, nil, err
		}
	}

	s.logger.InfoContext(r.ctx, "created channel group",
		"channel_group_id", dbGroupID,
		"user_group_id", group.ID,
		"name", group.Name,
		"channel_count", len(group.ChannelIDs),
		"subscriber_count", len(initialMembers),
	)
	return &CreateChannelGroupResponse{
		Response:       successResponse(),
		ChannelGroupID: dbGroupID,
	}, nil, nil
}

func (s *channelGroups) GetChannelGroups(ctx context.Context) GetChannelGroupsRequest {
	return GetChannelGroupsRequest{ctx: ctx, apiService: s}
}

func (s *channelGroups) GetChannelGroupsExecute(
	r GetChannelGroupsRequest,
) (*GetChannelGroupsResponse, *http.Response, error) {
	groups, err := s.getGroups(r.ctx)
	if err != nil {
		return nil, nil, err
	}
	groups, err = s.withUserGroupNames(r.ctx, groups)
	if err != nil {
		return nil, nil, err
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return &GetChannelGroupsResponse{
		Response:      successResponse(),
		ChannelGroups: groups,
	}, nil, nil
}

func (s *channelGroups) GetChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupRequest {
	return GetChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) GetChannelGroupExecute(
	r GetChannelGroupRequest,
) (*GetChannelGroupResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	group, err = s.withUserGroupName(r.ctx, group)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupResponse{
		Response:     successResponse(),
		ChannelGroup: group,
	}, nil, nil
}

func (s *channelGroups) GetChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupChannelsRequest {
	return GetChannelGroupChannelsRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) GetChannelGroupChannelsExecute(
	r GetChannelGroupChannelsRequest,
) (*GetChannelGroupChannelsResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupChannelsResponse{
		Response:   successResponse(),
		ChannelIDs: group.ChannelIDs,
	}, nil, nil
}

func (s *channelGroups) GetIsChannelInChannelGroup(
	ctx context.Context,
	channelGroupID int64,
	channelID int64,
) GetIsChannelInChannelGroupRequest {
	return GetIsChannelInChannelGroupRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
		channelID:      channelID,
	}
}

func (s *channelGroups) GetIsChannelInChannelGroupExecute(
	r GetIsChannelInChannelGroupRequest,
) (*GetIsChannelInChannelGroupResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelInChannelGroupResponse{
		Response:             successResponse(),
		IsChannelGroupMember: containsInt64(group.ChannelIDs, r.channelID),
	}, nil, nil
}

func (s *channelGroups) UpdateChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) UpdateChannelGroupChannelsRequest {
	return UpdateChannelGroupChannelsRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) UpdateChannelGroupChannelsExecute(
	r UpdateChannelGroupChannelsRequest,
) (*zulip.Response, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originUpdateChannels)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	added := idsToAdd(group.ChannelIDs, r.addChannelIDs)
	deleted := idsToDelete(group.ChannelIDs, r.deleteChannelIDs)
	members, err := s.userGroupMembersForChannelUpdates(r.ctx, group.ID, added, deleted)
	if err != nil {
		return nil, nil, err
	}
	if len(added) > 0 && len(members) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, added, members); err != nil {
			return nil, nil, err
		}
	}

	finalState, err := s.updateGroupChannels(r.ctx, r.channelGroupID, r.addChannelIDs, r.deleteChannelIDs)
	if err != nil {
		return nil, nil, err
	}
	if err = s.cleanupAddedChannels(r.ctx, added, members, group.ID, finalState.ChannelIDs); err != nil {
		return nil, nil, err
	}
	if err = s.reconcileChannelGroup(r.ctx, r.channelGroupID); err != nil {
		return nil, nil, err
	}

	s.logger.InfoContext(r.ctx, "updated channel group channels",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"added_channel_ids", added,
		"deleted_channel_ids", deleted,
		"channel_count", len(finalState.ChannelIDs),
		"subscriber_count", len(members),
	)
	return ptrResponse(successResponse()), nil, nil
}

func (s *channelGroups) GetChannelGroupSubscribers(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupSubscribersRequest {
	return GetChannelGroupSubscribersRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) GetChannelGroupSubscribersExecute(
	r GetChannelGroupSubscribersRequest,
) (*GetChannelGroupSubscribersResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	members, err := s.userGroupMembers(r.ctx, group.ID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupSubscribersResponse{
		Response:      successResponse(),
		SubscriberIDs: members,
	}, nil, nil
}

func (s *channelGroups) GetIsChannelGroupSubscriber(
	ctx context.Context,
	channelGroupID int64,
	userID int64,
) GetIsChannelGroupSubscriberRequest {
	return GetIsChannelGroupSubscriberRequest{
		ctx:            ctx,
		apiService:     s,
		channelGroupID: channelGroupID,
		userID:         userID,
	}
}

func (s *channelGroups) GetIsChannelGroupSubscriberExecute(
	r GetIsChannelGroupSubscriberRequest,
) (*GetIsChannelGroupSubscriberResponse, *http.Response, error) {
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	resp, _, err := s.base.GetIsUserGroupMember(r.ctx, group.ID, r.userID).DirectMemberOnly(true).Execute()
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelGroupSubscriberResponse{
		Response:     successResponse(),
		IsSubscriber: resp.IsUserGroupMember,
	}, nil, nil
}

func (s *channelGroups) SubscribeToChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) SubscribeToChannelGroupRequest {
	return SubscribeToChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) SubscribeToChannelGroupExecute(
	r SubscribeToChannelGroupRequest,
) (*SubscribeToChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originSubscribe)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	if len(group.ChannelIDs) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, userIDs); err != nil {
			return nil, nil, err
		}
	}
	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, group.ID).Add(userIDs).Execute()
	if err != nil {
		_ = s.unsubscribeUsersFromChannels(r.ctx, group.ChannelIDs, userIDs)
		return nil, nil, err
	}
	if err = s.reconcileChannelGroup(r.ctx, r.channelGroupID); err != nil {
		return nil, nil, err
	}
	latestState, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	s.logger.InfoContext(r.ctx, "subscribed users to channel group",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"user_ids", userIDs,
		"channel_count", len(latestState.ChannelIDs),
	)
	return &SubscribeToChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *channelGroups) UnsubscribeFromChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) UnsubscribeFromChannelGroupRequest {
	return UnsubscribeFromChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *channelGroups) UnsubscribeFromChannelGroupExecute(
	r UnsubscribeFromChannelGroupRequest,
) (*UnsubscribeFromChannelGroupResponse, *http.Response, error) {
	r.ctx = callorigin.With(r.ctx, originUnsubscribe)
	group, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	if len(group.ChannelIDs) > 0 {
		if err = s.unsubscribeUsersFromChannels(r.ctx, group.ChannelIDs, userIDs); err != nil {
			return nil, nil, err
		}
	}
	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, group.ID).Delete(userIDs).Execute()
	if err != nil {
		_ = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, userIDs)
		return nil, nil, err
	}
	finalState, err := s.getGroup(r.ctx, r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	addedWhileUnsubscribing := removeInt64s(finalState.ChannelIDs, group.ChannelIDs)
	if len(addedWhileUnsubscribing) > 0 {
		s.logger.DebugContext(r.ctx, "removing unsubscribed users from channels added concurrently",
			"channel_group_id", r.channelGroupID,
			"channel_ids", addedWhileUnsubscribing,
			"user_ids", userIDs,
		)
		if err = s.unsubscribeUsersFromChannels(r.ctx, addedWhileUnsubscribing, userIDs); err != nil {
			return nil, nil, err
		}
	}

	s.logger.InfoContext(r.ctx, "unsubscribed users from channel group",
		"channel_group_id", r.channelGroupID,
		"user_group_id", group.ID,
		"user_ids", userIDs,
		"channel_count", len(finalState.ChannelIDs),
	)
	return &UnsubscribeFromChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *channelGroups) getGroup(ctx context.Context, channelGroupID int64) (ChannelGroup, error) {
	dbGroupID, err := s.queries.GetChannelGroup(ctx, channelGroupID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ChannelGroup{}, errChannelGroupNotFound(channelGroupID)
		}
		return ChannelGroup{}, err
	}
	channels, err := s.queries.ListChannelGroupChannels(ctx, channelGroupID)
	if err != nil {
		return ChannelGroup{}, err
	}
	return channelGroupFromDB(dbGroupID, channels), nil
}

func (s *channelGroups) getGroups(ctx context.Context) ([]ChannelGroup, error) {
	dbGroupIDs, err := s.queries.ListChannelGroups(ctx)
	if err != nil {
		return nil, err
	}

	groups := make([]ChannelGroup, 0, len(dbGroupIDs))
	for _, dbGroupID := range dbGroupIDs {
		var channels []int64
		channels, err = s.queries.ListChannelGroupChannels(ctx, dbGroupID)
		if err != nil {
			return nil, err
		}
		groups = append(groups, channelGroupFromDB(dbGroupID, channels))
	}
	return groups, nil
}

func (s *channelGroups) updateGroupChannels(
	ctx context.Context,
	channelGroupID int64,
	addChannelIDs *[]int64,
	deleteChannelIDs *[]int64,
) (ChannelGroup, error) {
	if _, err := s.getGroup(ctx, channelGroupID); err != nil {
		return ChannelGroup{}, err
	}
	if deleteChannelIDs != nil {
		for _, channelID := range uniqueInt64s(*deleteChannelIDs) {
			if err := s.queries.RemoveChannelGroupChannel(ctx, channelgroupdb.RemoveChannelGroupChannelParams{
				ChannelGroupID: channelGroupID,
				ChannelID:      channelID,
			}); err != nil {
				return ChannelGroup{}, err
			}
		}
	}
	if addChannelIDs != nil {
		for _, channelID := range uniqueInt64s(*addChannelIDs) {
			if err := s.queries.AddChannelGroupChannel(ctx, channelgroupdb.AddChannelGroupChannelParams{
				ChannelGroupID: channelGroupID,
				ChannelID:      channelID,
			}); err != nil {
				return ChannelGroup{}, err
			}
		}
	}
	return s.getGroup(ctx, channelGroupID)
}

func (s *channelGroups) cleanupAddedChannels(
	ctx context.Context,
	addedChannelIDs []int64,
	previousUserIDs []int64,
	userGroupID int64,
	currentChannelIDs []int64,
) error {
	channelsToClean := idsToDelete(currentChannelIDs, &addedChannelIDs)
	if len(channelsToClean) == 0 || len(previousUserIDs) == 0 {
		return nil
	}
	currentUserIDs, err := s.userGroupMembers(ctx, userGroupID)
	if err != nil {
		return err
	}
	staleUserIDs := removeInt64s(previousUserIDs, currentUserIDs)
	if len(staleUserIDs) == 0 {
		return nil
	}
	s.logger.DebugContext(ctx, "cleaning channels added during channel group membership change",
		"channel_ids", channelsToClean,
		"user_ids", staleUserIDs,
	)
	return s.unsubscribeUsersFromChannels(ctx, channelsToClean, staleUserIDs)
}

func (s *channelGroups) reconcileChannelGroup(ctx context.Context, channelGroupID int64) error {
	group, err := s.getGroup(ctx, channelGroupID)
	if err != nil {
		return err
	}
	if len(group.ChannelIDs) == 0 {
		return nil
	}
	members, err := s.userGroupMembers(ctx, group.ID)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	s.logger.DebugContext(ctx, "reconciling channel group subscriptions",
		"channel_group_id", channelGroupID,
		"user_group_id", group.ID,
		"channel_count", len(group.ChannelIDs),
		"subscriber_count", len(members),
	)
	return s.subscribeUsersToChannels(ctx, group.ChannelIDs, members)
}

func (s *channelGroups) subscribeUsersToChannels(
	ctx context.Context,
	channelIDs []int64,
	userIDs []int64,
) error {
	subscriptions, err := s.subscriptionRequests(ctx, channelIDs)
	if err != nil {
		return err
	}
	_, _, err = s.base.Subscribe(ctx).
		Subscriptions(subscriptions).
		Principals(zulip.UserIDsAsPrincipals(userIDs...)).
		Execute()
	return err
}

func (s *channelGroups) unsubscribeUsersFromChannels(
	ctx context.Context,
	channelIDs []int64,
	userIDs []int64,
) error {
	channelNames, err := s.channelNames(ctx, channelIDs)
	if err != nil {
		return err
	}
	_, _, err = s.base.Unsubscribe(ctx).
		Subscriptions(channelNames).
		Principals(zulip.UserIDsAsPrincipals(userIDs...)).
		Execute()
	return err
}

func (s *channelGroups) userGroupMembersForChannelUpdates(
	ctx context.Context,
	userGroupID int64,
	added []int64,
	deleted []int64,
) ([]int64, error) {
	if len(added) == 0 && len(deleted) == 0 {
		return nil, nil
	}
	return s.userGroupMembers(ctx, userGroupID)
}

func (s *channelGroups) subscriptionRequests(
	ctx context.Context,
	channelIDs []int64,
) ([]channels.SubscriptionRequest, error) {
	channelNames, err := s.channelNames(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	subscriptions := make([]channels.SubscriptionRequest, 0, len(channelNames))
	for _, name := range channelNames {
		subscriptions = append(subscriptions, channels.SubscriptionRequest{Name: name})
	}
	return subscriptions, nil
}

func (s *channelGroups) channelNames(ctx context.Context, channelIDs []int64) ([]string, error) {
	names := make([]string, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		resp, _, err := s.base.GetChannelByID(ctx, channelID).Execute()
		if err != nil {
			return nil, err
		}
		names = append(names, resp.Channel.Name)
	}
	return names, nil
}

func (s *channelGroups) userGroupMembers(ctx context.Context, userGroupID int64) ([]int64, error) {
	resp, _, err := s.base.GetUserGroupMembers(ctx, userGroupID).DirectMemberOnly(true).Execute()
	if err != nil {
		return nil, err
	}
	return uniqueInt64s(resp.Members), nil
}

func (s *channelGroups) userGroupsByID(ctx context.Context) (map[int64]zulip.UserGroup, error) {
	resp, _, err := s.base.GetUserGroups(ctx).IncludeDeactivatedGroups(false).Execute()
	if err != nil {
		return nil, err
	}
	groups := make(map[int64]zulip.UserGroup, len(resp.UserGroups))
	for _, group := range resp.UserGroups {
		groups[group.ID] = group
	}
	return groups, nil
}

func (s *channelGroups) subscribedChannelIDs(ctx context.Context) (map[int64]struct{}, error) {
	resp, _, err := s.base.GetSubscriptions(ctx).Execute()
	if err != nil {
		return nil, err
	}
	channelIDs := make(map[int64]struct{}, len(resp.Subscriptions))
	for _, subscription := range resp.Subscriptions {
		channelIDs[subscription.ChannelID] = struct{}{}
	}
	return channelIDs, nil
}

func (s *channelGroups) withUserGroupName(ctx context.Context, group ChannelGroup) (ChannelGroup, error) {
	groups, err := s.withUserGroupNames(ctx, []ChannelGroup{group})
	if err != nil {
		return ChannelGroup{}, err
	}
	return groups[0], nil
}

func (s *channelGroups) withUserGroupNames(ctx context.Context, groups []ChannelGroup) ([]ChannelGroup, error) {
	if len(groups) == 0 {
		return groups, nil
	}
	resp, _, err := s.base.GetUserGroups(ctx).IncludeDeactivatedGroups(true).Execute()
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(resp.UserGroups))
	for _, userGroup := range resp.UserGroups {
		names[userGroup.ID] = userGroup.Name
	}
	hydrated := make([]ChannelGroup, 0, len(groups))
	for _, group := range groups {
		name, ok := names[group.ID]
		if !ok {
			return nil, errChannelGroupNotFound(group.ID)
		}
		group = cloneChannelGroup(group)
		group.Name = name
		hydrated = append(hydrated, group)
	}
	return hydrated, nil
}

func channelGroupFromDB(id int64, channelIDs []int64) ChannelGroup {
	return ChannelGroup{
		ID:         id,
		ChannelIDs: uniqueInt64s(channelIDs),
	}
}

func successResponse() zulip.Response {
	return zulip.Response{Result: zulip.ResponseSuccess}
}

func administratorsOnlyGroupSetting() zulip.GroupSettingValue {
	groupID := zulipAdministratorsSystemGroupID
	return zulip.GroupSettingValue{GroupID: &groupID}
}

func ptrResponse(r zulip.Response) *zulip.Response {
	return &r
}

func errChannelGroupNotFound(channelGroupID int64) error {
	return errors.New("channel group " + strconv.FormatInt(channelGroupID, 10) + " not found")
}

func cloneChannelGroup(group ChannelGroup) ChannelGroup {
	group.ChannelIDs = append([]int64(nil), group.ChannelIDs...)
	return group
}

func userIDPrincipals(principals *zulip.Principals) ([]int64, error) {
	if principals == nil {
		return nil, nil
	}
	if principals.UserEmails != nil && len(*principals.UserEmails) > 0 {
		return nil, errors.New("channel group operations only support user ID principals")
	}
	if principals.UserIDs == nil {
		return nil, nil
	}
	return uniqueInt64s(*principals.UserIDs), nil
}

func idsToAdd(existing []int64, requested *[]int64) []int64 {
	if requested == nil {
		return nil
	}
	added := make([]int64, 0, len(*requested))
	for _, id := range uniqueInt64s(*requested) {
		if !containsInt64(existing, id) {
			added = append(added, id)
		}
	}
	return added
}

func idsToDelete(existing []int64, requested *[]int64) []int64 {
	if requested == nil {
		return nil
	}
	deleted := make([]int64, 0, len(*requested))
	for _, id := range uniqueInt64s(*requested) {
		if containsInt64(existing, id) {
			deleted = append(deleted, id)
		}
	}
	return deleted
}

func uniqueInt64s(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	unique := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	return unique
}

func removeInt64s(values []int64, remove []int64) []int64 {
	removeSet := make(map[int64]struct{}, len(remove))
	for _, value := range remove {
		removeSet[value] = struct{}{}
	}
	remaining := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := removeSet[value]; !ok {
			remaining = append(remaining, value)
		}
	}
	return uniqueInt64s(remaining)
}

func containsInt64(values []int64, needle int64) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// APIChannelGroups is the set of endpoints provided by the channel-group
// service. Method shape (Builder / Execute split, context on the
// constructor) matches the upstream go-zulip conventions.
type APIChannelGroups interface {
	// InitializeChannelGroups reconciles persisted channel groups against
	// Zulip state at startup. It removes stale metadata only; it does not
	// unsubscribe users from channels.
	InitializeChannelGroups(ctx context.Context) error

	// CreateChannelGroup creates a new channel group, optionally
	// pre-populated with channels and initial subscribers.
	CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest
	CreateChannelGroupExecute(r CreateChannelGroupRequest) (*CreateChannelGroupResponse, *http.Response, error)

	// GetChannelGroups lists channel groups visible to the acting user.
	GetChannelGroups(ctx context.Context) GetChannelGroupsRequest
	GetChannelGroupsExecute(r GetChannelGroupsRequest) (*GetChannelGroupsResponse, *http.Response, error)

	// GetChannelGroup fetches a single channel group by ID.
	GetChannelGroup(ctx context.Context, channelGroupID int64) GetChannelGroupRequest
	GetChannelGroupExecute(r GetChannelGroupRequest) (*GetChannelGroupResponse, *http.Response, error)

	// --- Channel membership inside a group --------------------------------
	// "Members" of a channel group are channels. Subscribers are tracked
	// in the backing Zulip user group.

	GetChannelGroupChannels(ctx context.Context, channelGroupID int64) GetChannelGroupChannelsRequest
	GetChannelGroupChannelsExecute(
		r GetChannelGroupChannelsRequest,
	) (*GetChannelGroupChannelsResponse, *http.Response, error)

	GetIsChannelInChannelGroup(
		ctx context.Context,
		channelGroupID int64,
		channelID int64,
	) GetIsChannelInChannelGroupRequest
	GetIsChannelInChannelGroupExecute(
		r GetIsChannelInChannelGroupRequest,
	) (*GetIsChannelInChannelGroupResponse, *http.Response, error)

	// UpdateChannelGroupChannels adds and/or removes channels in a single
	// operation.
	UpdateChannelGroupChannels(ctx context.Context, channelGroupID int64) UpdateChannelGroupChannelsRequest
	UpdateChannelGroupChannelsExecute(r UpdateChannelGroupChannelsRequest) (*zulip.Response, *http.Response, error)

	// --- Subscribers ------------------------------------------------------
	// Subscribing a user (principal) to a channel group materializes
	// subscriptions to every channel currently in the group. Unsubscribing
	// removes them from every channel in the group.

	GetChannelGroupSubscribers(ctx context.Context, channelGroupID int64) GetChannelGroupSubscribersRequest
	GetChannelGroupSubscribersExecute(
		r GetChannelGroupSubscribersRequest,
	) (*GetChannelGroupSubscribersResponse, *http.Response, error)

	GetIsChannelGroupSubscriber(
		ctx context.Context,
		channelGroupID int64,
		userID int64,
	) GetIsChannelGroupSubscriberRequest
	GetIsChannelGroupSubscriberExecute(
		r GetIsChannelGroupSubscriberRequest,
	) (*GetIsChannelGroupSubscriberResponse, *http.Response, error)

	SubscribeToChannelGroup(ctx context.Context, channelGroupID int64) SubscribeToChannelGroupRequest
	SubscribeToChannelGroupExecute(
		r SubscribeToChannelGroupRequest,
	) (*SubscribeToChannelGroupResponse, *http.Response, error)

	UnsubscribeFromChannelGroup(ctx context.Context, channelGroupID int64) UnsubscribeFromChannelGroupRequest
	UnsubscribeFromChannelGroupExecute(
		r UnsubscribeFromChannelGroupRequest,
	) (*UnsubscribeFromChannelGroupResponse, *http.Response, error)
}

// =============================================================================
// Domain model
// =============================================================================

// ChannelGroup is the wire representation of a channel group as returned by
// the server. Field names follow the conventions used by zulip.UserGroup.
type ChannelGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// ChannelIDs are the channels currently in the group.
	ChannelIDs []int64 `json:"channel_ids"`
}

// =============================================================================
// Request builders
//
// Only the *shape* of the builders is declared here. Each builder is expected
// to expose: chainable setters that return the builder by value, and an
// Execute() method that delegates to the matching ...Execute on the service.
// =============================================================================

// --- Create -----------------------------------------------------------------

type CreateChannelGroupRequest struct {
	ctx                context.Context
	apiService         APIChannelGroups
	name               *string
	channelIDs         *[]int64
	initialSubscribers *zulip.Principals
}

func (r CreateChannelGroupRequest) Name(name string) CreateChannelGroupRequest {
	r.name = &name
	return r
}

func (r CreateChannelGroupRequest) ChannelIDs(ids []int64) CreateChannelGroupRequest {
	r.channelIDs = &ids
	return r
}

func (r CreateChannelGroupRequest) InitialSubscribers(p zulip.Principals) CreateChannelGroupRequest {
	r.initialSubscribers = &p
	return r
}

func (r CreateChannelGroupRequest) Execute() (*CreateChannelGroupResponse, *http.Response, error) {
	return r.apiService.CreateChannelGroupExecute(r)
}

type CreateChannelGroupResponse struct {
	zulip.Response

	ChannelGroupID int64 `json:"channel_group_id"`
}

// --- Read -------------------------------------------------------------------

type GetChannelGroupsRequest struct {
	ctx        context.Context
	apiService APIChannelGroups
}

func (r GetChannelGroupsRequest) Execute() (*GetChannelGroupsResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupsExecute(r)
}

type GetChannelGroupsResponse struct {
	zulip.Response

	ChannelGroups []ChannelGroup `json:"channel_groups"`
}

type GetChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupRequest) Execute() (*GetChannelGroupResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupExecute(r)
}

type GetChannelGroupResponse struct {
	zulip.Response

	ChannelGroup ChannelGroup `json:"channel_group"`
}

// --- Channels in a group ----------------------------------------------------

type GetChannelGroupChannelsRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupChannelsRequest) Execute() (*GetChannelGroupChannelsResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupChannelsExecute(r)
}

type GetChannelGroupChannelsResponse struct {
	zulip.Response

	ChannelIDs []int64 `json:"channel_ids"`
}

type GetIsChannelInChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	channelID      int64
}

func (r GetIsChannelInChannelGroupRequest) Execute() (*GetIsChannelInChannelGroupResponse, *http.Response, error) {
	return r.apiService.GetIsChannelInChannelGroupExecute(r)
}

type GetIsChannelInChannelGroupResponse struct {
	zulip.Response

	IsChannelGroupMember bool `json:"is_channel_group_member"`
}

type UpdateChannelGroupChannelsRequest struct {
	ctx              context.Context
	apiService       APIChannelGroups
	channelGroupID   int64
	addChannelIDs    *[]int64
	deleteChannelIDs *[]int64
}

// Add specifies channels to add to the group. The server is expected to
// subscribe every current group subscriber to each added channel.
func (r UpdateChannelGroupChannelsRequest) Add(channelIDs []int64) UpdateChannelGroupChannelsRequest {
	r.addChannelIDs = &channelIDs
	return r
}

// Delete specifies channels to remove from the group. The server is expected
// to unsubscribe every current group subscriber from each removed channel
// (unless they are subscribed to it via another path).
func (r UpdateChannelGroupChannelsRequest) Delete(channelIDs []int64) UpdateChannelGroupChannelsRequest {
	r.deleteChannelIDs = &channelIDs
	return r
}

func (r UpdateChannelGroupChannelsRequest) Execute() (*zulip.Response, *http.Response, error) {
	return r.apiService.UpdateChannelGroupChannelsExecute(r)
}

// --- Subscribers ------------------------------------------------------------

type GetChannelGroupSubscribersRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r GetChannelGroupSubscribersRequest) Execute() (*GetChannelGroupSubscribersResponse, *http.Response, error) {
	return r.apiService.GetChannelGroupSubscribersExecute(r)
}

type GetChannelGroupSubscribersResponse struct {
	zulip.Response

	SubscriberIDs []int64 `json:"subscriber_ids"`
}

type GetIsChannelGroupSubscriberRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	userID         int64
}

func (r GetIsChannelGroupSubscriberRequest) Execute() (*GetIsChannelGroupSubscriberResponse, *http.Response, error) {
	return r.apiService.GetIsChannelGroupSubscriberExecute(r)
}

type GetIsChannelGroupSubscriberResponse struct {
	zulip.Response

	IsSubscriber bool `json:"is_subscriber"`
}

type SubscribeToChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	principals     *zulip.Principals
}

// Principals selects which users to subscribe. This implementation requires
// user ID principals so it can update the backing Zulip user group.
func (r SubscribeToChannelGroupRequest) Principals(p zulip.Principals) SubscribeToChannelGroupRequest {
	r.principals = &p
	return r
}

func (r SubscribeToChannelGroupRequest) Execute() (*SubscribeToChannelGroupResponse, *http.Response, error) {
	return r.apiService.SubscribeToChannelGroupExecute(r)
}

type SubscribeToChannelGroupResponse struct {
	zulip.Response
}

type UnsubscribeFromChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
	principals     *zulip.Principals
}

// Principals selects which users to unsubscribe. This implementation requires
// user ID principals so it can update the backing Zulip user group.
func (r UnsubscribeFromChannelGroupRequest) Principals(p zulip.Principals) UnsubscribeFromChannelGroupRequest {
	r.principals = &p
	return r
}

func (r UnsubscribeFromChannelGroupRequest) Execute() (*UnsubscribeFromChannelGroupResponse, *http.Response, error) {
	return r.apiService.UnsubscribeFromChannelGroupExecute(r)
}

type UnsubscribeFromChannelGroupResponse struct {
	zulip.Response
}
