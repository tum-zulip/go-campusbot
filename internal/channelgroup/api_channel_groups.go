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
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	"github.com/tum-zulip/go-zulip/zulip/client"
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

// NewClient wraps an existing upstream client.Client with channel-group
// endpoints. The wrapper does not own the underlying client's lifecycle.
func NewClient(base client.Client) Client {
	service := newInMemoryChannelGroups(base)
	return &channelGroupClient{
		Client:           base,
		APIChannelGroups: service,
	}
}

type inMemoryChannelGroups struct {
	base client.Client

	mu     sync.Mutex
	nextID int64
	groups map[int64]channelGroupState
}

type channelGroupState struct {
	ChannelGroup

	userGroupID int64
}

func newInMemoryChannelGroups(base client.Client) *inMemoryChannelGroups {
	return &inMemoryChannelGroups{
		base:   base,
		nextID: 1,
		groups: make(map[int64]channelGroupState),
	}
}

var _ APIChannelGroups = (*inMemoryChannelGroups)(nil)

func (s *inMemoryChannelGroups) CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest {
	return CreateChannelGroupRequest{ctx: ctx, apiService: s}
}

func (s *inMemoryChannelGroups) CreateChannelGroupExecute(
	r CreateChannelGroupRequest,
) (*CreateChannelGroupResponse, *http.Response, error) {
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
	if r.description != nil {
		group.Description = *r.description
	}
	if r.channelIDs != nil {
		group.ChannelIDs = uniqueInt64s(*r.channelIDs)
	}
	group.DirectSubscriberIDs = initialMembers

	userGroupResp, _, err := s.base.CreateUserGroup(r.ctx).
		Name(group.Name).
		Description(group.Description).
		Members(initialMembers).
		Execute()
	if err != nil {
		return nil, nil, err
	}

	if len(group.ChannelIDs) > 0 && len(initialMembers) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, group.ChannelIDs, initialMembers); err != nil {
			return nil, nil, err
		}
	}

	s.mu.Lock()
	group.ID = s.nextID
	s.nextID++
	s.groups[group.ID] = channelGroupState{
		ChannelGroup: cloneChannelGroup(group),
		userGroupID:  userGroupResp.GroupID,
	}
	s.mu.Unlock()

	return &CreateChannelGroupResponse{
		Response:       successResponse(),
		ChannelGroupID: group.ID,
	}, nil, nil
}

func (s *inMemoryChannelGroups) DeactivateChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) DeactivateChannelGroupRequest {
	return DeactivateChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) DeactivateChannelGroupExecute(
	r DeactivateChannelGroupRequest,
) (*zulip.Response, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	if state.Deactivated {
		return nil, nil, errChannelGroupDeactivated(r.channelGroupID)
	}
	if _, _, err := s.base.DeactivateUserGroup(r.ctx, state.userGroupID).Execute(); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.groups[r.channelGroupID]
	if !ok {
		return nil, nil, errChannelGroupNotFound(r.channelGroupID)
	}
	state.Deactivated = true
	s.groups[r.channelGroupID] = state
	return ptrResponse(successResponse()), nil, nil
}

func (s *inMemoryChannelGroups) GetChannelGroups(ctx context.Context) GetChannelGroupsRequest {
	return GetChannelGroupsRequest{ctx: ctx, apiService: s}
}

func (s *inMemoryChannelGroups) GetChannelGroupsExecute(
	r GetChannelGroupsRequest,
) (*GetChannelGroupsResponse, *http.Response, error) {
	includeDeactivated := r.includeDeactivated != nil && *r.includeDeactivated
	states := s.getGroupStates(includeDeactivated)
	groups := make([]ChannelGroup, 0, len(states))
	for _, state := range states {
		group := cloneChannelGroup(state.ChannelGroup)
		members, err := s.userGroupMembers(r.ctx, state.userGroupID)
		if err != nil {
			return nil, nil, err
		}
		group.DirectSubscriberIDs = members
		groups = append(groups, group)
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return &GetChannelGroupsResponse{
		Response:      successResponse(),
		ChannelGroups: groups,
	}, nil, nil
}

func (s *inMemoryChannelGroups) GetChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupRequest {
	return GetChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) GetChannelGroupExecute(
	r GetChannelGroupRequest,
) (*GetChannelGroupResponse, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	group := cloneChannelGroup(state.ChannelGroup)
	members, err := s.userGroupMembers(r.ctx, state.userGroupID)
	if err != nil {
		return nil, nil, err
	}
	group.DirectSubscriberIDs = members
	return &GetChannelGroupResponse{
		Response:     successResponse(),
		ChannelGroup: group,
	}, nil, nil
}

func (s *inMemoryChannelGroups) GetChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupChannelsRequest {
	return GetChannelGroupChannelsRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) GetChannelGroupChannelsExecute(
	r GetChannelGroupChannelsRequest,
) (*GetChannelGroupChannelsResponse, *http.Response, error) {
	group, err := s.getGroup(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupChannelsResponse{
		Response:   successResponse(),
		ChannelIDs: group.ChannelIDs,
	}, nil, nil
}

func (s *inMemoryChannelGroups) GetIsChannelInChannelGroup(
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

func (s *inMemoryChannelGroups) GetIsChannelInChannelGroupExecute(
	r GetIsChannelInChannelGroupRequest,
) (*GetIsChannelInChannelGroupResponse, *http.Response, error) {
	group, err := s.getGroup(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelInChannelGroupResponse{
		Response:             successResponse(),
		IsChannelGroupMember: containsInt64(group.ChannelIDs, r.channelID),
	}, nil, nil
}

func (s *inMemoryChannelGroups) UpdateChannelGroupChannels(
	ctx context.Context,
	channelGroupID int64,
) UpdateChannelGroupChannelsRequest {
	return UpdateChannelGroupChannelsRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) UpdateChannelGroupChannelsExecute(
	r UpdateChannelGroupChannelsRequest,
) (*zulip.Response, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	if state.Deactivated {
		return nil, nil, errChannelGroupDeactivated(r.channelGroupID)
	}

	added := idsToAdd(state.ChannelIDs, r.addChannelIDs)
	deleted := idsToDelete(state.ChannelIDs, r.deleteChannelIDs)
	members, err := s.userGroupMembersForChannelUpdates(r.ctx, state.userGroupID, added, deleted)
	if err != nil {
		return nil, nil, err
	}
	if len(added) > 0 && len(members) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, added, members); err != nil {
			return nil, nil, err
		}
	}
	if len(deleted) > 0 && len(members) > 0 {
		if err = s.unsubscribeUsersFromChannels(r.ctx, deleted, members); err != nil {
			return nil, nil, err
		}
	}

	finalState, err := s.updateGroupChannels(r.channelGroupID, r.addChannelIDs, r.deleteChannelIDs)
	if err != nil {
		return nil, nil, err
	}
	if err = s.cleanupDeletedChannels(r.ctx, deleted, members, finalState.ChannelIDs); err != nil {
		return nil, nil, err
	}
	if err = s.cleanupAddedChannels(r.ctx, added, members, state.userGroupID, finalState.ChannelIDs); err != nil {
		return nil, nil, err
	}
	if err = s.reconcileChannelGroup(r.ctx, r.channelGroupID); err != nil {
		return nil, nil, err
	}

	return ptrResponse(successResponse()), nil, nil
}

func (s *inMemoryChannelGroups) GetChannelGroupSubscribers(
	ctx context.Context,
	channelGroupID int64,
) GetChannelGroupSubscribersRequest {
	return GetChannelGroupSubscribersRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) GetChannelGroupSubscribersExecute(
	r GetChannelGroupSubscribersRequest,
) (*GetChannelGroupSubscribersResponse, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	members, err := s.userGroupMembers(r.ctx, state.userGroupID)
	if err != nil {
		return nil, nil, err
	}
	return &GetChannelGroupSubscribersResponse{
		Response:      successResponse(),
		SubscriberIDs: members,
	}, nil, nil
}

func (s *inMemoryChannelGroups) GetIsChannelGroupSubscriber(
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

func (s *inMemoryChannelGroups) GetIsChannelGroupSubscriberExecute(
	r GetIsChannelGroupSubscriberRequest,
) (*GetIsChannelGroupSubscriberResponse, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	resp, _, err := s.base.GetIsUserGroupMember(r.ctx, state.userGroupID, r.userID).DirectMemberOnly(true).Execute()
	if err != nil {
		return nil, nil, err
	}
	return &GetIsChannelGroupSubscriberResponse{
		Response:     successResponse(),
		IsSubscriber: resp.IsUserGroupMember,
	}, nil, nil
}

func (s *inMemoryChannelGroups) SubscribeToChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) SubscribeToChannelGroupRequest {
	return SubscribeToChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) SubscribeToChannelGroupExecute(
	r SubscribeToChannelGroupRequest,
) (*SubscribeToChannelGroupResponse, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	if state.Deactivated {
		return nil, nil, errChannelGroupDeactivated(r.channelGroupID)
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	if len(state.ChannelIDs) > 0 {
		if err = s.subscribeUsersToChannels(r.ctx, state.ChannelIDs, userIDs); err != nil {
			return nil, nil, err
		}
	}
	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, state.userGroupID).Add(userIDs).Execute()
	if err != nil {
		_ = s.unsubscribeUsersFromChannels(r.ctx, state.ChannelIDs, userIDs)
		return nil, nil, err
	}
	finalState, err := s.updateDirectSubscribers(r.channelGroupID, userIDs, nil)
	if err != nil {
		return nil, nil, err
	}
	staleChannels := removeInt64s(state.ChannelIDs, finalState.ChannelIDs)
	if len(staleChannels) > 0 {
		if err = s.unsubscribeUsersFromChannels(r.ctx, staleChannels, userIDs); err != nil {
			return nil, nil, err
		}
	}
	if err = s.reconcileChannelGroup(r.ctx, r.channelGroupID); err != nil {
		return nil, nil, err
	}

	return &SubscribeToChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *inMemoryChannelGroups) UnsubscribeFromChannelGroup(
	ctx context.Context,
	channelGroupID int64,
) UnsubscribeFromChannelGroupRequest {
	return UnsubscribeFromChannelGroupRequest{ctx: ctx, apiService: s, channelGroupID: channelGroupID}
}

func (s *inMemoryChannelGroups) UnsubscribeFromChannelGroupExecute(
	r UnsubscribeFromChannelGroupRequest,
) (*UnsubscribeFromChannelGroupResponse, *http.Response, error) {
	state, err := s.getGroupState(r.channelGroupID)
	if err != nil {
		return nil, nil, err
	}
	if state.Deactivated {
		return nil, nil, errChannelGroupDeactivated(r.channelGroupID)
	}

	userIDs, err := userIDPrincipals(r.principals)
	if err != nil {
		return nil, nil, err
	}
	if len(userIDs) == 0 {
		return nil, nil, errors.New("principals with user IDs are required")
	}

	if len(state.ChannelIDs) > 0 {
		if err = s.unsubscribeUsersFromChannels(r.ctx, state.ChannelIDs, userIDs); err != nil {
			return nil, nil, err
		}
	}
	_, _, err = s.base.UpdateUserGroupMembers(r.ctx, state.userGroupID).Delete(userIDs).Execute()
	if err != nil {
		_ = s.subscribeUsersToChannels(r.ctx, state.ChannelIDs, userIDs)
		return nil, nil, err
	}
	finalState, err := s.updateDirectSubscribers(r.channelGroupID, nil, userIDs)
	if err != nil {
		return nil, nil, err
	}
	addedWhileUnsubscribing := removeInt64s(finalState.ChannelIDs, state.ChannelIDs)
	if len(addedWhileUnsubscribing) > 0 {
		if err = s.unsubscribeUsersFromChannels(r.ctx, addedWhileUnsubscribing, userIDs); err != nil {
			return nil, nil, err
		}
	}

	return &UnsubscribeFromChannelGroupResponse{
		Response: successResponse(),
	}, nil, nil
}

func (s *inMemoryChannelGroups) getGroup(channelGroupID int64) (ChannelGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.groups[channelGroupID]
	if !ok {
		return ChannelGroup{}, errChannelGroupNotFound(channelGroupID)
	}
	return cloneChannelGroup(state.ChannelGroup), nil
}

func (s *inMemoryChannelGroups) getGroupState(channelGroupID int64) (channelGroupState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.groups[channelGroupID]
	if !ok {
		return channelGroupState{}, errChannelGroupNotFound(channelGroupID)
	}
	return cloneChannelGroupState(state), nil
}

func (s *inMemoryChannelGroups) getGroupStates(includeDeactivated bool) []channelGroupState {
	s.mu.Lock()
	defer s.mu.Unlock()

	states := make([]channelGroupState, 0, len(s.groups))
	for _, state := range s.groups {
		if state.Deactivated && !includeDeactivated {
			continue
		}
		states = append(states, cloneChannelGroupState(state))
	}
	return states
}

func (s *inMemoryChannelGroups) updateGroupChannels(
	channelGroupID int64,
	addChannelIDs *[]int64,
	deleteChannelIDs *[]int64,
) (channelGroupState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.groups[channelGroupID]
	if !ok {
		return channelGroupState{}, errChannelGroupNotFound(channelGroupID)
	}
	if state.Deactivated {
		return channelGroupState{}, errChannelGroupDeactivated(channelGroupID)
	}
	if deleteChannelIDs != nil {
		state.ChannelIDs = removeInt64s(state.ChannelIDs, uniqueInt64s(*deleteChannelIDs))
	}
	if addChannelIDs != nil {
		state.ChannelIDs = uniqueInt64s(append(state.ChannelIDs, uniqueInt64s(*addChannelIDs)...))
	}
	s.groups[channelGroupID] = state
	return cloneChannelGroupState(state), nil
}

func (s *inMemoryChannelGroups) updateDirectSubscribers(
	channelGroupID int64,
	addUserIDs []int64,
	deleteUserIDs []int64,
) (channelGroupState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.groups[channelGroupID]
	if !ok {
		return channelGroupState{}, errChannelGroupNotFound(channelGroupID)
	}
	if state.Deactivated {
		return channelGroupState{}, errChannelGroupDeactivated(channelGroupID)
	}
	if len(deleteUserIDs) > 0 {
		state.DirectSubscriberIDs = removeInt64s(state.DirectSubscriberIDs, deleteUserIDs)
	}
	if len(addUserIDs) > 0 {
		state.DirectSubscriberIDs = uniqueInt64s(append(state.DirectSubscriberIDs, addUserIDs...))
	}
	s.groups[channelGroupID] = state
	return cloneChannelGroupState(state), nil
}

func (s *inMemoryChannelGroups) cleanupDeletedChannels(
	ctx context.Context,
	deletedChannelIDs []int64,
	userIDs []int64,
	currentChannelIDs []int64,
) error {
	channelsToClean := removeInt64s(uniqueInt64s(deletedChannelIDs), currentChannelIDs)
	if len(channelsToClean) == 0 || len(userIDs) == 0 {
		return nil
	}
	return s.unsubscribeUsersFromChannels(ctx, channelsToClean, userIDs)
}

func (s *inMemoryChannelGroups) cleanupAddedChannels(
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
	return s.unsubscribeUsersFromChannels(ctx, channelsToClean, staleUserIDs)
}

func (s *inMemoryChannelGroups) reconcileChannelGroup(ctx context.Context, channelGroupID int64) error {
	state, err := s.getGroupState(channelGroupID)
	if err != nil {
		return err
	}
	if state.Deactivated || len(state.ChannelIDs) == 0 {
		return nil
	}
	members, err := s.userGroupMembers(ctx, state.userGroupID)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	return s.subscribeUsersToChannels(ctx, state.ChannelIDs, members)
}

func (s *inMemoryChannelGroups) subscribeUsersToChannels(
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

func (s *inMemoryChannelGroups) unsubscribeUsersFromChannels(
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

func (s *inMemoryChannelGroups) userGroupMembersForChannelUpdates(
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

func (s *inMemoryChannelGroups) subscriptionRequests(
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

func (s *inMemoryChannelGroups) channelNames(ctx context.Context, channelIDs []int64) ([]string, error) {
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

func (s *inMemoryChannelGroups) userGroupMembers(ctx context.Context, userGroupID int64) ([]int64, error) {
	resp, _, err := s.base.GetUserGroupMembers(ctx, userGroupID).DirectMemberOnly(true).Execute()
	if err != nil {
		return nil, err
	}
	return uniqueInt64s(resp.Members), nil
}

func successResponse() zulip.Response {
	return zulip.Response{Result: zulip.ResponseSuccess}
}

func ptrResponse(r zulip.Response) *zulip.Response {
	return &r
}

func errChannelGroupNotFound(channelGroupID int64) error {
	return errors.New("channel group " + strconv.FormatInt(channelGroupID, 10) + " not found")
}

func errChannelGroupDeactivated(channelGroupID int64) error {
	return errors.New("channel group " + strconv.FormatInt(channelGroupID, 10) + " is deactivated")
}

func cloneChannelGroup(group ChannelGroup) ChannelGroup {
	group.ChannelIDs = append([]int64(nil), group.ChannelIDs...)
	group.DirectSubscriberIDs = append([]int64(nil), group.DirectSubscriberIDs...)
	return group
}

func cloneChannelGroupState(state channelGroupState) channelGroupState {
	state.ChannelGroup = cloneChannelGroup(state.ChannelGroup)
	return state
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
	// CreateChannelGroup creates a new channel group, optionally
	// pre-populated with channels and initial subscribers.
	CreateChannelGroup(ctx context.Context) CreateChannelGroupRequest
	CreateChannelGroupExecute(r CreateChannelGroupRequest) (*CreateChannelGroupResponse, *http.Response, error)

	// DeactivateChannelGroup deactivates a channel group. Existing
	// subscribers retain their direct subscriptions to the channels
	// that were in the group; the group itself becomes inactive.
	DeactivateChannelGroup(ctx context.Context, channelGroupID int64) DeactivateChannelGroupRequest
	DeactivateChannelGroupExecute(r DeactivateChannelGroupRequest) (*zulip.Response, *http.Response, error)

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
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`

	// ChannelIDs are the channels currently in the group.
	ChannelIDs []int64 `json:"channel_ids"`

	// DirectSubscriberIDs are mirrored from the backing Zulip user group.
	DirectSubscriberIDs []int64 `json:"direct_subscriber_ids"`

	Deactivated bool `json:"deactivated"`
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
	description        *string
	channelIDs         *[]int64
	initialSubscribers *zulip.Principals
}

func (r CreateChannelGroupRequest) Name(name string) CreateChannelGroupRequest {
	r.name = &name
	return r
}

func (r CreateChannelGroupRequest) Description(d string) CreateChannelGroupRequest {
	r.description = &d
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

// --- Deactivate -------------------------------------------------------------

type DeactivateChannelGroupRequest struct {
	ctx            context.Context
	apiService     APIChannelGroups
	channelGroupID int64
}

func (r DeactivateChannelGroupRequest) Execute() (*zulip.Response, *http.Response, error) {
	return r.apiService.DeactivateChannelGroupExecute(r)
}

// --- Read -------------------------------------------------------------------

type GetChannelGroupsRequest struct {
	ctx                context.Context
	apiService         APIChannelGroups
	includeDeactivated *bool
}

// IncludeDeactivated filters in deactivated groups, mirroring upstream
// "include_deactivated" flags.
func (r GetChannelGroupsRequest) IncludeDeactivated(b bool) GetChannelGroupsRequest {
	r.includeDeactivated = &b
	return r
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
