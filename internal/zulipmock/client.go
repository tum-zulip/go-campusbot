// Code generated for tests; DO NOT EDIT.
package zulipmock

import (
	"context"
	"fmt"
	"github.com/tum-zulip/go-campusbot/internal/callorigin"
	"github.com/tum-zulip/go-zulip/zulip"
	"github.com/tum-zulip/go-zulip/zulip/api/authentication"
	"github.com/tum-zulip/go-zulip/zulip/api/channels"
	"github.com/tum-zulip/go-zulip/zulip/api/drafts"
	"github.com/tum-zulip/go-zulip/zulip/api/invites"
	"github.com/tum-zulip/go-zulip/zulip/api/messages"
	"github.com/tum-zulip/go-zulip/zulip/api/mobile"
	navigationviews "github.com/tum-zulip/go-zulip/zulip/api/navigation_views"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	"github.com/tum-zulip/go-zulip/zulip/api/reminders"
	scheduledmessages "github.com/tum-zulip/go-zulip/zulip/api/scheduled_messages"
	serverandorganizations "github.com/tum-zulip/go-zulip/zulip/api/server_and_organizations"
	"github.com/tum-zulip/go-zulip/zulip/api/users"
	"github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/client/statistics"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

type Client struct {
	state *state
}

type Operation string

const (
	OperationCreateUserGroup        Operation = "CreateUserGroup"
	OperationDeactivateUserGroup    Operation = "DeactivateUserGroup"
	OperationGetChannelByID         Operation = "GetChannelByID"
	OperationGetSubscribers         Operation = "GetSubscribers"
	OperationGetIsUserGroupMember   Operation = "GetIsUserGroupMember"
	OperationGetUserGroupMembers    Operation = "GetUserGroupMembers"
	OperationGetUserGroups          Operation = "GetUserGroups"
	OperationSubscribe              Operation = "Subscribe"
	OperationUnsubscribe            Operation = "Unsubscribe"
	OperationUpdateUserGroupMembers Operation = "UpdateUserGroupMembers"
)

type state struct {
	mu              sync.Mutex
	nextChannelID   int64
	nextUserGroupID int64
	channels        map[int64]channelState
	channelIDs      map[string]int64
	userGroups      map[int64]userGroupState
	failures        map[Operation][]error
	serialization   *RequestSerialization
}

type RequestSerialization struct {
	mu     sync.Mutex
	cond   *sync.Cond
	order  []RequestStep
	next   int
	closed bool
}

type RequestStep struct {
	Operation Operation
	Key       string
	// Origin optionally restricts the step to calls issued under a matching
	// callorigin tag (see internal/callorigin). An empty Origin matches any
	// origin, preserving compatibility with tests that don't care.
	Origin string
}

// From returns a copy of step restricted to calls tagged with origin.
func (s RequestStep) From(origin string) RequestStep {
	s.Origin = origin
	return s
}

func OperationRequest(op Operation) RequestStep {
	return RequestStep{Operation: op}
}

func ChannelRequest(op Operation, channelID int64) RequestStep {
	return RequestStep{Operation: op, Key: int64Key(channelID)}
}

func SubscriptionRequest(op Operation, channelNames []string, userIDs []int64) RequestStep {
	return RequestStep{Operation: op, Key: subscriptionKey(channelNames, userIDs)}
}

func UserGroupMembersRequest(userGroupID int64) RequestStep {
	return RequestStep{Operation: OperationGetUserGroupMembers, Key: int64Key(userGroupID)}
}

func UpdateUserGroupMembersRequest(userGroupID int64, add []int64, del []int64) RequestStep {
	return RequestStep{Operation: OperationUpdateUserGroupMembers, Key: updateUserGroupMembersPartsKey(userGroupID, add, del)}
}

type channelState struct {
	channel     zulip.Channel
	subscribers map[int64]bool
}

type userGroupState struct {
	group zulip.UserGroup
}

var _ client.Client = Client{}

func NewClient() Client {
	return Client{state: &state{
		nextChannelID:   1,
		nextUserGroupID: 1,
		channels:        map[int64]channelState{},
		channelIDs:      map[string]int64{},
		userGroups:      map[int64]userGroupState{},
		failures:        map[Operation][]error{},
	}}
}
func (Client) GetStatistics() statistics.Statistics { return statistics.Statistics{} }

func (c Client) ensureState() *state {
	if c.state != nil {
		return c.state
	}
	return NewClient().state
}

func (c Client) SerializeRequests(order ...Operation) *RequestSerialization {
	steps := make([]RequestStep, 0, len(order))
	for _, op := range order {
		steps = append(steps, RequestStep{Operation: op})
	}
	return c.SerializeRequestSteps(steps...)
}

func (c Client) SerializeRequestSteps(order ...RequestStep) *RequestSerialization {
	serialization := &RequestSerialization{order: append([]RequestStep(nil), order...)}
	serialization.cond = sync.NewCond(&serialization.mu)

	state := c.ensureState()
	state.mu.Lock()
	state.serialization = serialization
	state.mu.Unlock()
	return serialization
}

func (c Client) ClearRequestSerialization() {
	state := c.ensureState()
	state.mu.Lock()
	serialization := state.serialization
	state.serialization = nil
	state.mu.Unlock()
	if serialization != nil {
		serialization.Close()
	}
}

func (c Client) FailNext(op Operation, err error) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	if err == nil {
		err = fmt.Errorf("%s failed", op)
	}
	state.failures[op] = append(state.failures[op], err)
}

func (s *state) failLocked(op Operation) error {
	failures := s.failures[op]
	if len(failures) == 0 {
		return nil
	}
	err := failures[0]
	if len(failures) == 1 {
		delete(s.failures, op)
	} else {
		s.failures[op] = failures[1:]
	}
	return err
}

func (s *state) waitForTurn(ctx context.Context, op Operation, key string) error {
	s.mu.Lock()
	serialization := s.serialization
	s.mu.Unlock()
	if serialization == nil {
		return nil
	}
	return serialization.waitForTurn(ctx, RequestStep{Operation: op, Key: key, Origin: callorigin.From(ctx)})
}

func (s *RequestSerialization) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	s.cond.Broadcast()
}

func (s *RequestSerialization) Wait(ctx context.Context) error {
	return s.WaitForSteps(ctx, len(s.order))
}

func (s *RequestSerialization) WaitForSteps(ctx context.Context, steps int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if steps > len(s.order) {
		steps = len(s.order)
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	s.mu.Lock()
	defer s.mu.Unlock()
	for !s.closed && s.next < steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.cond.Wait()
	}
	return ctx.Err()
}

func (s *RequestSerialization) waitForTurn(ctx context.Context, step RequestStep) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-done:
		}
	}()
	defer close(done)

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed || s.next >= len(s.order) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if requestStepMatches(s.order[s.next], step) {
			s.next++
			s.cond.Broadcast()
			return nil
		}
		s.cond.Wait()
	}
}

func requestStepMatches(expected RequestStep, actual RequestStep) bool {
	if expected.Operation != actual.Operation {
		return false
	}
	if expected.Key != "" && expected.Key != actual.Key {
		return false
	}
	if expected.Origin != "" && expected.Origin != actual.Origin {
		return false
	}
	return true
}

func int64Key(value int64) string {
	return fmt.Sprintf("%d", value)
}

func userGroupMemberKey[T any](request T) string {
	return int64Key(requestInt64(request, "userGroupID")) + "/" + int64Key(requestInt64(request, "userID"))
}

func subscribeKey(request channels.SubscribeRequest) string {
	subscriptions := requestSubscriptionsPtr(request)
	if subscriptions == nil {
		return subscriptionKey(nil, principalUserIDs(requestPrincipalsPtr(request, "principals")))
	}
	names := make([]string, 0, len(*subscriptions))
	for _, subscription := range *subscriptions {
		names = append(names, subscription.Name)
	}
	return subscriptionKey(names, principalUserIDs(requestPrincipalsPtr(request, "principals")))
}

func unsubscribeKey(request channels.UnsubscribeRequest) string {
	subscriptions := requestSubscriptionNamesPtr(request)
	if subscriptions == nil {
		return subscriptionKey(nil, principalUserIDs(requestPrincipalsPtr(request, "principals")))
	}
	return subscriptionKey(*subscriptions, principalUserIDs(requestPrincipalsPtr(request, "principals")))
}

func subscriptionKey(channelNames []string, userIDs []int64) string {
	names := append([]string(nil), channelNames...)
	sort.Strings(names)
	return "channels=" + strings.Join(names, ",") + ";users=" + int64ListKey(userIDs)
}

func updateUserGroupMembersKey(request users.UpdateUserGroupMembersRequest) string {
	add := []int64(nil)
	if values := requestInt64SlicePtr(request, "add"); values != nil {
		add = *values
	}
	del := []int64(nil)
	if values := requestInt64SlicePtr(request, "delete"); values != nil {
		del = *values
	}
	return updateUserGroupMembersPartsKey(requestInt64(request, "userGroupID"), add, del)
}

func updateUserGroupMembersPartsKey(userGroupID int64, add []int64, del []int64) string {
	return "group=" + int64Key(userGroupID) + ";add=" + int64ListKey(add) + ";delete=" + int64ListKey(del)
}

func int64ListKey(values []int64) string {
	values = sortedMemberIDs(values)
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, int64Key(value))
	}
	return strings.Join(parts, ",")
}

func withAPIService[T any](request T, service Client) T {
	v := reflect.ValueOf(&request).Elem()
	field := v.FieldByName("apiService")
	if !field.IsValid() || !field.CanAddr() {
		return request
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(service))
	return request
}

func withContext[T any](request T, ctx context.Context) T {
	if ctx == nil {
		ctx = context.Background()
	}
	v := reflect.ValueOf(&request).Elem()
	field := v.FieldByName("ctx")
	if !field.IsValid() || !field.CanAddr() {
		return request
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(ctx))
	return request
}

func withInt64Field[T any](request T, name string, value int64) T {
	v := reflect.ValueOf(&request).Elem()
	field := v.FieldByName(name)
	if !field.IsValid() || !field.CanAddr() {
		return request
	}
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().SetInt(value)
	return request
}

func requestInt64[T any](request T, name string) int64 {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	return field.Int()
}

func requestContext[T any](request T) context.Context {
	v := reflect.ValueOf(&request).Elem()
	field := v.FieldByName("ctx")
	if !field.IsValid() || field.IsNil() {
		return context.Background()
	}
	ctx, ok := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface().(context.Context)
	if !ok || ctx == nil {
		return context.Background()
	}
	return ctx
}

func requestStringPtr[T any](request T, name string) *string {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*string)(unsafe.Pointer(field.Pointer()))
}

func requestInt64SlicePtr[T any](request T, name string) *[]int64 {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*[]int64)(unsafe.Pointer(field.Pointer()))
}

func requestPrincipalsPtr[T any](request T, name string) *zulip.Principals {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.Principals)(unsafe.Pointer(field.Pointer()))
}

func requestSubscriptionsPtr(request channels.SubscribeRequest) *[]channels.SubscriptionRequest {
	v := reflect.ValueOf(request)
	field := v.FieldByName("subscriptions")
	if field.IsNil() {
		return nil
	}
	return (*[]channels.SubscriptionRequest)(unsafe.Pointer(field.Pointer()))
}

func requestSubscriptionNamesPtr(request channels.UnsubscribeRequest) *[]string {
	v := reflect.ValueOf(request)
	field := v.FieldByName("subscriptions")
	if field.IsNil() {
		return nil
	}
	return (*[]string)(unsafe.Pointer(field.Pointer()))
}

func principalUserIDs(p *zulip.Principals) []int64 {
	if p == nil || p.UserIDs == nil {
		return []int64{0}
	}
	return append([]int64(nil), (*p.UserIDs)...)
}

func sortedMemberIDs(members []int64) []int64 {
	out := append([]int64(nil), members...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func successResponse() zulip.Response {
	return zulip.Response{Result: zulip.ResponseSuccess}
}
func (Client) AddAlertWords(_arg0 context.Context) users.AddAlertWordsRequest {
	return withAPIService(users.AddAlertWordsRequest{}, Client{})
}
func (Client) AddAlertWordsExecute(_arg0 users.AddAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddApnsToken(_arg0 context.Context) users.AddApnsTokenRequest {
	return withAPIService(users.AddApnsTokenRequest{}, Client{})
}
func (Client) AddApnsTokenExecute(_arg0 users.AddApnsTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddCodePlayground(_arg0 context.Context) serverandorganizations.AddCodePlaygroundRequest {
	return withAPIService(serverandorganizations.AddCodePlaygroundRequest{}, Client{})
}
func (Client) AddCodePlaygroundExecute(_arg0 serverandorganizations.AddCodePlaygroundRequest) (*serverandorganizations.AddCodePlaygroundResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddDefaultChannel(_arg0 context.Context) channels.AddDefaultChannelRequest {
	return withAPIService(channels.AddDefaultChannelRequest{}, Client{})
}
func (Client) AddDefaultChannelExecute(_arg0 channels.AddDefaultChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddFcmToken(_arg0 context.Context) users.AddFcmTokenRequest {
	return withAPIService(users.AddFcmTokenRequest{}, Client{})
}
func (Client) AddFcmTokenExecute(_arg0 users.AddFcmTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddLinkifier(_arg0 context.Context) serverandorganizations.AddLinkifierRequest {
	return withAPIService(serverandorganizations.AddLinkifierRequest{}, Client{})
}
func (Client) AddLinkifierExecute(_arg0 serverandorganizations.AddLinkifierRequest) (*serverandorganizations.AddLinkifierResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddNavigationView(_arg0 context.Context) navigationviews.AddNavigationViewRequest {
	return withAPIService(navigationviews.AddNavigationViewRequest{}, Client{})
}
func (Client) AddNavigationViewExecute(_arg0 navigationviews.AddNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddReaction(_arg0 context.Context, _arg1 int64) messages.AddReactionRequest {
	return withAPIService(messages.AddReactionRequest{}, Client{})
}
func (Client) AddReactionExecute(_arg0 messages.AddReactionRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ArchiveChannel(_arg0 context.Context, _arg1 int64) channels.ArchiveChannelRequest {
	return withAPIService(channels.ArchiveChannelRequest{}, Client{})
}
func (Client) ArchiveChannelExecute(_arg0 channels.ArchiveChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CheckMessagesMatchNarrow(_arg0 context.Context) messages.CheckMessagesMatchNarrowRequest {
	return withAPIService(messages.CheckMessagesMatchNarrowRequest{}, Client{})
}
func (Client) CheckMessagesMatchNarrowExecute(_arg0 messages.CheckMessagesMatchNarrowRequest) (*messages.CheckMessagesMatchNarrowResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateBigBlueButtonVideoCall(_arg0 context.Context) channels.CreateBigBlueButtonVideoCallRequest {
	return withAPIService(channels.CreateBigBlueButtonVideoCallRequest{}, Client{})
}
func (Client) CreateBigBlueButtonVideoCallExecute(_arg0 channels.CreateBigBlueButtonVideoCallRequest) (*channels.CreateBigBlueButtonVideoCallResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateChannel(_arg0 context.Context) channels.CreateChannelRequest {
	return withAPIService(channels.CreateChannelRequest{}, Client{})
}
func (Client) CreateChannelExecute(_arg0 channels.CreateChannelRequest) (*channels.CreateChannelResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateChannelFolder(_arg0 context.Context) channels.CreateChannelFolderRequest {
	return withAPIService(channels.CreateChannelFolderRequest{}, Client{})
}
func (Client) CreateChannelFolderExecute(_arg0 channels.CreateChannelFolderRequest) (*channels.CreateChannelFolderResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateCustomProfileField(_arg0 context.Context) serverandorganizations.CreateCustomProfileFieldRequest {
	return withAPIService(serverandorganizations.CreateCustomProfileFieldRequest{}, Client{})
}
func (Client) CreateCustomProfileFieldExecute(_arg0 serverandorganizations.CreateCustomProfileFieldRequest) (*serverandorganizations.CreateCustomProfileFieldResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateDrafts(_arg0 context.Context) drafts.CreateDraftsRequest {
	return withAPIService(drafts.CreateDraftsRequest{}, Client{})
}
func (Client) CreateDraftsExecute(_arg0 drafts.CreateDraftsRequest) (*drafts.CreateDraftsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateInviteLink(_arg0 context.Context) invites.CreateInviteLinkRequest {
	return withAPIService(invites.CreateInviteLinkRequest{}, Client{})
}
func (Client) CreateInviteLinkExecute(_arg0 invites.CreateInviteLinkRequest) (*invites.CreateInviteLinkResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateMessageReminder(_arg0 context.Context) reminders.CreateMessageReminderRequest {
	return withAPIService(reminders.CreateMessageReminderRequest{}, Client{})
}
func (Client) CreateMessageReminderExecute(_arg0 reminders.CreateMessageReminderRequest) (*reminders.CreateMessageReminderResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateSavedSnippet(_arg0 context.Context) drafts.CreateSavedSnippetRequest {
	return withAPIService(drafts.CreateSavedSnippetRequest{}, Client{})
}
func (Client) CreateSavedSnippetExecute(_arg0 drafts.CreateSavedSnippetRequest) (*drafts.CreateSavedSnippetResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateScheduledMessage(_arg0 context.Context) scheduledmessages.CreateScheduledMessageRequest {
	return withAPIService(scheduledmessages.CreateScheduledMessageRequest{}, Client{})
}
func (Client) CreateScheduledMessageExecute(_arg0 scheduledmessages.CreateScheduledMessageRequest) (*scheduledmessages.CreateScheduledMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateUser(_arg0 context.Context) users.CreateUserRequest {
	return withAPIService(users.CreateUserRequest{}, Client{})
}
func (Client) CreateUserExecute(_arg0 users.CreateUserRequest) (*users.CreateUserResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) CreateUserGroup(ctx context.Context) users.CreateUserGroupRequest {
	return withContext(withAPIService(users.CreateUserGroupRequest{}, c), ctx)
}
func (c Client) CreateUserGroupExecute(r users.CreateUserGroupRequest) (*users.CreateUserGroupResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationCreateUserGroup, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationCreateUserGroup); err != nil {
		return nil, nil, err
	}

	name := ""
	if v := requestStringPtr(r, "name"); v != nil {
		name = *v
	}
	if name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	description := ""
	if v := requestStringPtr(r, "description"); v != nil {
		description = *v
	} else {
		return nil, nil, fmt.Errorf("description is required")
	}
	members := []int64(nil)
	if v := requestInt64SlicePtr(r, "members"); v != nil {
		members = sortedMemberIDs(*v)
	} else {
		return nil, nil, fmt.Errorf("members is required")
	}
	subgroups := []int64(nil)
	if v := requestInt64SlicePtr(r, "subgroups"); v != nil {
		subgroups = sortedMemberIDs(*v)
	}

	id := state.nextUserGroupID
	state.nextUserGroupID++
	state.userGroups[id] = userGroupState{group: zulip.UserGroup{
		ID:                id,
		Name:              name,
		Description:       description,
		Members:           members,
		DirectSubgroupIDs: subgroups,
	}}

	return &users.CreateUserGroupResponse{Response: successResponse(), GroupID: id}, nil, nil
}
func (Client) DeactivateCustomEmoji(_arg0 context.Context, _arg1 string) serverandorganizations.DeactivateCustomEmojiRequest {
	return withAPIService(serverandorganizations.DeactivateCustomEmojiRequest{}, Client{})
}
func (Client) DeactivateCustomEmojiExecute(_arg0 serverandorganizations.DeactivateCustomEmojiRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeactivateOwnUser(_arg0 context.Context) users.DeactivateOwnUserRequest {
	return withAPIService(users.DeactivateOwnUserRequest{}, Client{})
}
func (Client) DeactivateOwnUserExecute(_arg0 users.DeactivateOwnUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeactivateUser(_arg0 context.Context, _arg1 int64) users.DeactivateUserRequest {
	return withAPIService(users.DeactivateUserRequest{}, Client{})
}
func (Client) DeactivateUserExecute(_arg0 users.DeactivateUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) DeactivateUserGroup(ctx context.Context, userGroupID int64) users.DeactivateUserGroupRequest {
	return withInt64Field(withContext(withAPIService(users.DeactivateUserGroupRequest{}, c), ctx), "userGroupID", userGroupID)
}
func (c Client) DeactivateUserGroupExecute(r users.DeactivateUserGroupRequest) (*zulip.Response, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationDeactivateUserGroup, int64Key(requestInt64(r, "userGroupID"))); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationDeactivateUserGroup); err != nil {
		return nil, nil, err
	}

	id := requestInt64(r, "userGroupID")
	group, ok := state.userGroups[id]
	if !ok {
		return nil, nil, fmt.Errorf("user group %d not found", id)
	}
	group.group.Deactivated = true
	state.userGroups[id] = group
	resp := successResponse()
	return &resp, nil, nil
}
func (Client) DeleteDraft(_arg0 context.Context, _arg1 int64) drafts.DeleteDraftRequest {
	return withAPIService(drafts.DeleteDraftRequest{}, Client{})
}
func (Client) DeleteDraftExecute(_arg0 drafts.DeleteDraftRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteMessage(_arg0 context.Context, _arg1 int64) messages.DeleteMessageRequest {
	return withAPIService(messages.DeleteMessageRequest{}, Client{})
}
func (Client) DeleteMessageExecute(_arg0 messages.DeleteMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteQueue(_arg0 context.Context) realtimeevents.DeleteQueueRequest {
	return withAPIService(realtimeevents.DeleteQueueRequest{}, Client{})
}
func (Client) DeleteQueueExecute(_arg0 realtimeevents.DeleteQueueRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteReminder(_arg0 context.Context, _arg1 int64) reminders.DeleteReminderRequest {
	return withAPIService(reminders.DeleteReminderRequest{}, Client{})
}
func (Client) DeleteReminderExecute(_arg0 reminders.DeleteReminderRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteSavedSnippet(_arg0 context.Context, _arg1 int64) drafts.DeleteSavedSnippetRequest {
	return withAPIService(drafts.DeleteSavedSnippetRequest{}, Client{})
}
func (Client) DeleteSavedSnippetExecute(_arg0 drafts.DeleteSavedSnippetRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteScheduledMessage(_arg0 context.Context, _arg1 int64) scheduledmessages.DeleteScheduledMessageRequest {
	return withAPIService(scheduledmessages.DeleteScheduledMessageRequest{}, Client{})
}
func (Client) DeleteScheduledMessageExecute(_arg0 scheduledmessages.DeleteScheduledMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteTopic(_arg0 context.Context, _arg1 int64) channels.DeleteTopicRequest {
	return withAPIService(channels.DeleteTopicRequest{}, Client{})
}
func (Client) DeleteTopicExecute(_arg0 channels.DeleteTopicRequest) (*channels.MarkAllAsReadResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DevFetchAPIKey(_arg0 context.Context) authentication.DevFetchAPIKeyRequest {
	return withAPIService(authentication.DevFetchAPIKeyRequest{}, Client{})
}
func (Client) DevFetchAPIKeyExecute(_arg0 authentication.DevFetchAPIKeyRequest) (*authentication.APIKeyResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) E2eeTestNotify(_arg0 context.Context) mobile.E2eeTestNotifyRequest {
	return withAPIService(mobile.E2eeTestNotifyRequest{}, Client{})
}
func (Client) E2eeTestNotifyExecute(_arg0 mobile.E2eeTestNotifyRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditDraft(_arg0 context.Context, _arg1 int64) drafts.EditDraftRequest {
	return withAPIService(drafts.EditDraftRequest{}, Client{})
}
func (Client) EditDraftExecute(_arg0 drafts.EditDraftRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditNavigationView(_arg0 context.Context, _arg1 string) navigationviews.EditNavigationViewRequest {
	return withAPIService(navigationviews.EditNavigationViewRequest{}, Client{})
}
func (Client) EditNavigationViewExecute(_arg0 navigationviews.EditNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditSavedSnippet(_arg0 context.Context, _arg1 int64) drafts.EditSavedSnippetRequest {
	return withAPIService(drafts.EditSavedSnippetRequest{}, Client{})
}
func (Client) EditSavedSnippetExecute(_arg0 drafts.EditSavedSnippetRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ExportRealm(_arg0 context.Context) serverandorganizations.ExportRealmRequest {
	return withAPIService(serverandorganizations.ExportRealmRequest{}, Client{})
}
func (Client) ExportRealmExecute(_arg0 serverandorganizations.ExportRealmRequest) (*serverandorganizations.ExportRealmResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) FetchAPIKey(_arg0 context.Context) authentication.FetchAPIKeyRequest {
	return withAPIService(authentication.FetchAPIKeyRequest{}, Client{})
}
func (Client) FetchAPIKeyExecute(_arg0 authentication.FetchAPIKeyRequest) (*authentication.APIKeyResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetAlertWords(_arg0 context.Context) users.GetAlertWordsRequest {
	return withAPIService(users.GetAlertWordsRequest{}, Client{})
}
func (Client) GetAlertWordsExecute(_arg0 users.GetAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetAttachments(_arg0 context.Context) users.GetAttachmentsRequest {
	return withAPIService(users.GetAttachmentsRequest{}, Client{})
}
func (Client) GetAttachmentsExecute(_arg0 users.GetAttachmentsRequest) (*users.GetAttachmentsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetChannelByID(ctx context.Context, channelID int64) channels.GetChannelByIDRequest {
	return withInt64Field(withContext(withAPIService(channels.GetChannelByIDRequest{}, c), ctx), "channelID", channelID)
}
func (c Client) GetChannelByIDExecute(r channels.GetChannelByIDRequest) (*channels.GetChannelResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetChannelByID, int64Key(requestInt64(r, "channelID"))); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetChannelByID); err != nil {
		return nil, nil, err
	}

	id := requestInt64(r, "channelID")
	channel, ok := state.channels[id]
	if !ok {
		return nil, nil, fmt.Errorf("channel %d not found", id)
	}
	return &channels.GetChannelResponse{Response: successResponse(), Channel: channel.channel}, nil, nil
}
func (Client) GetChannelEmailAddress(_arg0 context.Context, _arg1 int64) channels.GetChannelEmailAddressRequest {
	return withAPIService(channels.GetChannelEmailAddressRequest{}, Client{})
}
func (Client) GetChannelEmailAddressExecute(_arg0 channels.GetChannelEmailAddressRequest) (*channels.GetChannelEmailAddressResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetChannelFolders(_arg0 context.Context) channels.GetChannelFoldersRequest {
	return withAPIService(channels.GetChannelFoldersRequest{}, Client{})
}
func (Client) GetChannelFoldersExecute(_arg0 channels.GetChannelFoldersRequest) (*channels.GetChannelFoldersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetChannelID(_arg0 context.Context) channels.GetChannelIDRequest {
	return withAPIService(channels.GetChannelIDRequest{}, Client{})
}
func (Client) GetChannelIDExecute(_arg0 channels.GetChannelIDRequest) (*channels.GetChannelIDResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetChannelTopics(_arg0 context.Context, _arg1 int64) channels.GetChannelTopicsRequest {
	return withAPIService(channels.GetChannelTopicsRequest{}, Client{})
}
func (Client) GetChannelTopicsExecute(_arg0 channels.GetChannelTopicsRequest) (*channels.GetChannelTopicsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetChannels(_arg0 context.Context) channels.GetChannelsRequest {
	return withAPIService(channels.GetChannelsRequest{}, Client{})
}
func (Client) GetChannelsExecute(_arg0 channels.GetChannelsRequest) (*channels.GetChannelsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetCustomEmoji(_arg0 context.Context) serverandorganizations.GetCustomEmojiRequest {
	return withAPIService(serverandorganizations.GetCustomEmojiRequest{}, Client{})
}
func (Client) GetCustomEmojiExecute(_arg0 serverandorganizations.GetCustomEmojiRequest) (*serverandorganizations.GetCustomEmojiResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetCustomProfileFields(_arg0 context.Context) serverandorganizations.GetCustomProfileFieldsRequest {
	return withAPIService(serverandorganizations.GetCustomProfileFieldsRequest{}, Client{})
}
func (Client) GetCustomProfileFieldsExecute(_arg0 serverandorganizations.GetCustomProfileFieldsRequest) (*serverandorganizations.GetCustomProfileFieldsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetDrafts(_arg0 context.Context) drafts.GetDraftsRequest {
	return withAPIService(drafts.GetDraftsRequest{}, Client{})
}
func (Client) GetDraftsExecute(_arg0 drafts.GetDraftsRequest) (*drafts.GetDraftsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetEvents(_arg0 context.Context) realtimeevents.GetEventsRequest {
	return withAPIService(realtimeevents.GetEventsRequest{}, Client{})
}
func (Client) GetEventsExecute(_arg0 realtimeevents.GetEventsRequest) (*realtimeevents.GetEventsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetFileTemporaryURL(_arg0 context.Context, _arg1 int64, _arg2 string) messages.GetFileTemporaryURLRequest {
	return withAPIService(messages.GetFileTemporaryURLRequest{}, Client{})
}
func (Client) GetFileTemporaryURLExecute(_arg0 messages.GetFileTemporaryURLRequest) (*messages.GetFileTemporaryURLResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetInvites(_arg0 context.Context) invites.GetInvitesRequest {
	return withAPIService(invites.GetInvitesRequest{}, Client{})
}
func (Client) GetInvitesExecute(_arg0 invites.GetInvitesRequest) (*invites.GetInvitesResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetIsUserGroupMember(ctx context.Context, userGroupID int64, userID int64) users.GetIsUserGroupMemberRequest {
	r := withContext(withAPIService(users.GetIsUserGroupMemberRequest{}, c), ctx)
	r = withInt64Field(r, "userGroupID", userGroupID)
	return withInt64Field(r, "userID", userID)
}
func (c Client) GetIsUserGroupMemberExecute(r users.GetIsUserGroupMemberRequest) (*users.GetIsUserGroupMemberResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetIsUserGroupMember, userGroupMemberKey(r)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetIsUserGroupMember); err != nil {
		return nil, nil, err
	}

	groupID := requestInt64(r, "userGroupID")
	userID := requestInt64(r, "userID")
	group, ok := state.userGroups[groupID]
	if !ok {
		return nil, nil, fmt.Errorf("user group %d not found", groupID)
	}
	for _, memberID := range group.group.Members {
		if memberID == userID {
			return &users.GetIsUserGroupMemberResponse{Response: successResponse(), IsUserGroupMember: true}, nil, nil
		}
	}
	return &users.GetIsUserGroupMemberResponse{Response: successResponse()}, nil, nil
}
func (Client) GetLinkifiers(_arg0 context.Context) serverandorganizations.GetLinkifiersRequest {
	return withAPIService(serverandorganizations.GetLinkifiersRequest{}, Client{})
}
func (Client) GetLinkifiersExecute(_arg0 serverandorganizations.GetLinkifiersRequest) (*serverandorganizations.GetLinkifiersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessage(_arg0 context.Context, _arg1 int64) messages.GetMessageRequest {
	return withAPIService(messages.GetMessageRequest{}, Client{})
}
func (Client) GetMessageExecute(_arg0 messages.GetMessageRequest) (*messages.GetMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessageHistory(_arg0 context.Context, _arg1 int64) messages.GetMessageHistoryRequest {
	return withAPIService(messages.GetMessageHistoryRequest{}, Client{})
}
func (Client) GetMessageHistoryExecute(_arg0 messages.GetMessageHistoryRequest) (*messages.GetMessageHistoryResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessages(_arg0 context.Context) messages.GetMessagesRequest {
	return withAPIService(messages.GetMessagesRequest{}, Client{})
}
func (Client) GetMessagesExecute(_arg0 messages.GetMessagesRequest) (*messages.GetMessagesResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetNavigationViews(_arg0 context.Context) navigationviews.GetNavigationViewsRequest {
	return withAPIService(navigationviews.GetNavigationViewsRequest{}, Client{})
}
func (Client) GetNavigationViewsExecute(_arg0 navigationviews.GetNavigationViewsRequest) (*navigationviews.GetNavigationViewsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetOwnUser(_arg0 context.Context) users.GetOwnUserRequest {
	return withAPIService(users.GetOwnUserRequest{}, Client{})
}
func (Client) GetOwnUserExecute(_arg0 users.GetOwnUserRequest) (*users.GetOwnUserResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetPresence(_arg0 context.Context) serverandorganizations.GetPresenceRequest {
	return withAPIService(serverandorganizations.GetPresenceRequest{}, Client{})
}
func (Client) GetPresenceExecute(_arg0 serverandorganizations.GetPresenceRequest) (*serverandorganizations.GetPresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetReadReceipts(_arg0 context.Context, _arg1 int64) messages.GetReadReceiptsRequest {
	return withAPIService(messages.GetReadReceiptsRequest{}, Client{})
}
func (Client) GetReadReceiptsExecute(_arg0 messages.GetReadReceiptsRequest) (*messages.GetReadReceiptsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetRealmExportConsents(_arg0 context.Context) serverandorganizations.GetRealmExportConsentsRequest {
	return withAPIService(serverandorganizations.GetRealmExportConsentsRequest{}, Client{})
}
func (Client) GetRealmExportConsentsExecute(_arg0 serverandorganizations.GetRealmExportConsentsRequest) (*serverandorganizations.GetRealmExportConsentsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetRealmExports(_arg0 context.Context) serverandorganizations.GetRealmExportsRequest {
	return withAPIService(serverandorganizations.GetRealmExportsRequest{}, Client{})
}
func (Client) GetRealmExportsExecute(_arg0 serverandorganizations.GetRealmExportsRequest) (*serverandorganizations.GetRealmExportsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetReminders(_arg0 context.Context) reminders.GetRemindersRequest {
	return withAPIService(reminders.GetRemindersRequest{}, Client{})
}
func (Client) GetRemindersExecute(_arg0 reminders.GetRemindersRequest) (*reminders.GetRemindersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetSavedSnippets(_arg0 context.Context) drafts.GetSavedSnippetsRequest {
	return withAPIService(drafts.GetSavedSnippetsRequest{}, Client{})
}
func (Client) GetSavedSnippetsExecute(_arg0 drafts.GetSavedSnippetsRequest) (*drafts.GetSavedSnippetsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetScheduledMessages(_arg0 context.Context) scheduledmessages.GetScheduledMessagesRequest {
	return withAPIService(scheduledmessages.GetScheduledMessagesRequest{}, Client{})
}
func (Client) GetScheduledMessagesExecute(_arg0 scheduledmessages.GetScheduledMessagesRequest) (*scheduledmessages.GetScheduledMessagesResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetServerSettings(_arg0 context.Context) serverandorganizations.GetServerSettingsRequest {
	return withAPIService(serverandorganizations.GetServerSettingsRequest{}, Client{})
}
func (Client) GetServerSettingsExecute(_arg0 serverandorganizations.GetServerSettingsRequest) (*serverandorganizations.GetServerSettingsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetSubscribers(ctx context.Context, channelID int64) channels.GetSubscribersRequest {
	return withInt64Field(withContext(withAPIService(channels.GetSubscribersRequest{}, c), ctx), "channelID", channelID)
}
func (c Client) GetSubscribersExecute(r channels.GetSubscribersRequest) (*channels.GetSubscribersResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetSubscribers, int64Key(requestInt64(r, "channelID"))); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetSubscribers); err != nil {
		return nil, nil, err
	}

	id := requestInt64(r, "channelID")
	channel, ok := state.channels[id]
	if !ok {
		return nil, nil, fmt.Errorf("channel %d not found", id)
	}
	subscribers := make([]int64, 0, len(channel.subscribers))
	for userID := range channel.subscribers {
		subscribers = append(subscribers, userID)
	}
	return &channels.GetSubscribersResponse{
		Response:    successResponse(),
		Subscribers: sortedMemberIDs(subscribers),
	}, nil, nil
}
func (Client) GetSubscriptionStatus(_arg0 context.Context, _arg1 int64, _arg2 int64) channels.GetSubscriptionStatusRequest {
	return withAPIService(channels.GetSubscriptionStatusRequest{}, Client{})
}
func (Client) GetSubscriptionStatusExecute(_arg0 channels.GetSubscriptionStatusRequest) (*channels.GetSubscriptionStatusResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetSubscriptions(_arg0 context.Context) channels.GetSubscriptionsRequest {
	return withAPIService(channels.GetSubscriptionsRequest{}, Client{})
}
func (Client) GetSubscriptionsExecute(_arg0 channels.GetSubscriptionsRequest) (*channels.GetSubscriptionsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUser(_arg0 context.Context, _arg1 int64) users.GetUserRequest {
	return withAPIService(users.GetUserRequest{}, Client{})
}
func (Client) GetUserByEmail(_arg0 context.Context, _arg1 string) users.GetUserByEmailRequest {
	return withAPIService(users.GetUserByEmailRequest{}, Client{})
}
func (Client) GetUserByEmailExecute(_arg0 users.GetUserByEmailRequest) (*users.GetUserResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUserExecute(_arg0 users.GetUserRequest) (*users.GetUserResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetUserGroupMembers(ctx context.Context, userGroupID int64) users.GetUserGroupMembersRequest {
	return withInt64Field(withContext(withAPIService(users.GetUserGroupMembersRequest{}, c), ctx), "userGroupID", userGroupID)
}
func (c Client) GetUserGroupMembersExecute(r users.GetUserGroupMembersRequest) (*users.GetUserGroupMembersResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetUserGroupMembers, int64Key(requestInt64(r, "userGroupID"))); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetUserGroupMembers); err != nil {
		return nil, nil, err
	}

	id := requestInt64(r, "userGroupID")
	group, ok := state.userGroups[id]
	if !ok {
		return nil, nil, fmt.Errorf("user group %d not found", id)
	}
	return &users.GetUserGroupMembersResponse{
		Response: successResponse(),
		Members:  sortedMemberIDs(group.group.Members),
	}, nil, nil
}
func (Client) GetUserGroupSubgroups(_arg0 context.Context, _arg1 int64) users.GetUserGroupSubgroupsRequest {
	return withAPIService(users.GetUserGroupSubgroupsRequest{}, Client{})
}
func (Client) GetUserGroupSubgroupsExecute(_arg0 users.GetUserGroupSubgroupsRequest) (*users.GetUserGroupSubgroupsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetUserGroups(ctx context.Context) users.GetUserGroupsRequest {
	return withContext(withAPIService(users.GetUserGroupsRequest{}, c), ctx)
}
func (c Client) GetUserGroupsExecute(r users.GetUserGroupsRequest) (*users.GetUserGroupsResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetUserGroups, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetUserGroups); err != nil {
		return nil, nil, err
	}

	groups := make([]zulip.UserGroup, 0, len(state.userGroups))
	for _, group := range state.userGroups {
		groups = append(groups, group.group)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	return &users.GetUserGroupsResponse{
		Response:   successResponse(),
		UserGroups: groups,
	}, nil, nil
}
func (Client) GetUserPresence(_arg0 context.Context, _arg1 string) users.GetUserPresenceRequest {
	return withAPIService(users.GetUserPresenceRequest{}, Client{})
}
func (Client) GetUserPresenceExecute(_arg0 users.GetUserPresenceRequest) (*users.GetUserPresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUserStatus(_arg0 context.Context, _arg1 int64) users.GetUserStatusRequest {
	return withAPIService(users.GetUserStatusRequest{}, Client{})
}
func (Client) GetUserStatusExecute(_arg0 users.GetUserStatusRequest) (*users.GetUserStatusResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUsers(_arg0 context.Context) users.GetUsersRequest {
	return withAPIService(users.GetUsersRequest{}, Client{})
}
func (Client) GetUsersExecute(_arg0 users.GetUsersRequest) (*users.GetUsersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkAllAsRead(_arg0 context.Context) messages.MarkAllAsReadRequest {
	return withAPIService(messages.MarkAllAsReadRequest{}, Client{})
}
func (Client) MarkAllAsReadExecute(_arg0 messages.MarkAllAsReadRequest) (*messages.MarkAllAsReadResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkChannelAsRead(_arg0 context.Context) messages.MarkChannelAsReadRequest {
	return withAPIService(messages.MarkChannelAsReadRequest{}, Client{})
}
func (Client) MarkChannelAsReadExecute(_arg0 messages.MarkChannelAsReadRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkTopicAsRead(_arg0 context.Context) messages.MarkTopicAsReadRequest {
	return withAPIService(messages.MarkTopicAsReadRequest{}, Client{})
}
func (Client) MarkTopicAsReadExecute(_arg0 messages.MarkTopicAsReadRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MuteTopic(_arg0 context.Context) channels.MuteTopicRequest {
	return withAPIService(channels.MuteTopicRequest{}, Client{})
}
func (Client) MuteTopicExecute(_arg0 channels.MuteTopicRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MuteUser(_arg0 context.Context, _arg1 int64) users.MuteUserRequest {
	return withAPIService(users.MuteUserRequest{}, Client{})
}
func (Client) MuteUserExecute(_arg0 users.MuteUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) PatchChannelFolders(_arg0 context.Context) channels.PatchChannelFoldersRequest {
	return withAPIService(channels.PatchChannelFoldersRequest{}, Client{})
}
func (Client) PatchChannelFoldersExecute(_arg0 channels.PatchChannelFoldersRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReactivateUser(_arg0 context.Context, _arg1 int64) users.ReactivateUserRequest {
	return withAPIService(users.ReactivateUserRequest{}, Client{})
}
func (Client) ReactivateUserExecute(_arg0 users.ReactivateUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RegisterPushDevice(_arg0 context.Context) mobile.RegisterPushDeviceRequest {
	return withAPIService(mobile.RegisterPushDeviceRequest{}, Client{})
}
func (Client) RegisterPushDeviceExecute(_arg0 mobile.RegisterPushDeviceRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RegisterQueue(_arg0 context.Context) realtimeevents.RegisterQueueRequest {
	return withAPIService(realtimeevents.RegisterQueueRequest{}, Client{})
}
func (Client) RegisterQueueExecute(_arg0 realtimeevents.RegisterQueueRequest) (*realtimeevents.RegisterQueueResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveAlertWords(_arg0 context.Context) users.RemoveAlertWordsRequest {
	return withAPIService(users.RemoveAlertWordsRequest{}, Client{})
}
func (Client) RemoveAlertWordsExecute(_arg0 users.RemoveAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveApnsToken(_arg0 context.Context) users.RemoveApnsTokenRequest {
	return withAPIService(users.RemoveApnsTokenRequest{}, Client{})
}
func (Client) RemoveApnsTokenExecute(_arg0 users.RemoveApnsTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveAttachment(_arg0 context.Context, _arg1 int64) users.RemoveAttachmentRequest {
	return withAPIService(users.RemoveAttachmentRequest{}, Client{})
}
func (Client) RemoveAttachmentExecute(_arg0 users.RemoveAttachmentRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveCodePlayground(_arg0 context.Context, _arg1 int64) serverandorganizations.RemoveCodePlaygroundRequest {
	return withAPIService(serverandorganizations.RemoveCodePlaygroundRequest{}, Client{})
}
func (Client) RemoveCodePlaygroundExecute(_arg0 serverandorganizations.RemoveCodePlaygroundRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveDefaultChannel(_arg0 context.Context) channels.RemoveDefaultChannelRequest {
	return withAPIService(channels.RemoveDefaultChannelRequest{}, Client{})
}
func (Client) RemoveDefaultChannelExecute(_arg0 channels.RemoveDefaultChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveFcmToken(_arg0 context.Context) users.RemoveFcmTokenRequest {
	return withAPIService(users.RemoveFcmTokenRequest{}, Client{})
}
func (Client) RemoveFcmTokenExecute(_arg0 users.RemoveFcmTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveLinkifier(_arg0 context.Context, _arg1 int64) serverandorganizations.RemoveLinkifierRequest {
	return withAPIService(serverandorganizations.RemoveLinkifierRequest{}, Client{})
}
func (Client) RemoveLinkifierExecute(_arg0 serverandorganizations.RemoveLinkifierRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveNavigationView(_arg0 context.Context, _arg1 string) navigationviews.RemoveNavigationViewRequest {
	return withAPIService(navigationviews.RemoveNavigationViewRequest{}, Client{})
}
func (Client) RemoveNavigationViewExecute(_arg0 navigationviews.RemoveNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveReaction(_arg0 context.Context, _arg1 int64) messages.RemoveReactionRequest {
	return withAPIService(messages.RemoveReactionRequest{}, Client{})
}
func (Client) RemoveReactionExecute(_arg0 messages.RemoveReactionRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RenderMessage(_arg0 context.Context) messages.RenderMessageRequest {
	return withAPIService(messages.RenderMessageRequest{}, Client{})
}
func (Client) RenderMessageExecute(_arg0 messages.RenderMessageRequest) (*messages.RenderMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReorderCustomProfileFields(_arg0 context.Context) serverandorganizations.ReorderCustomProfileFieldsRequest {
	return withAPIService(serverandorganizations.ReorderCustomProfileFieldsRequest{}, Client{})
}
func (Client) ReorderCustomProfileFieldsExecute(_arg0 serverandorganizations.ReorderCustomProfileFieldsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReorderLinkifiers(_arg0 context.Context) serverandorganizations.ReorderLinkifiersRequest {
	return withAPIService(serverandorganizations.ReorderLinkifiersRequest{}, Client{})
}
func (Client) ReorderLinkifiersExecute(_arg0 serverandorganizations.ReorderLinkifiersRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReportMessage(_arg0 context.Context, _arg1 int64) messages.ReportMessageRequest {
	return withAPIService(messages.ReportMessageRequest{}, Client{})
}
func (Client) ReportMessageExecute(_arg0 messages.ReportMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ResendEmailInvite(_arg0 context.Context, _arg1 int64) invites.ResendEmailInviteRequest {
	return withAPIService(invites.ResendEmailInviteRequest{}, Client{})
}
func (Client) ResendEmailInviteExecute(_arg0 invites.ResendEmailInviteRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RevokeEmailInvite(_arg0 context.Context, _arg1 int64) invites.RevokeEmailInviteRequest {
	return withAPIService(invites.RevokeEmailInviteRequest{}, Client{})
}
func (Client) RevokeEmailInviteExecute(_arg0 invites.RevokeEmailInviteRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RevokeInviteLink(_arg0 context.Context, _arg1 int64) invites.RevokeInviteLinkRequest {
	return withAPIService(invites.RevokeInviteLinkRequest{}, Client{})
}
func (Client) RevokeInviteLinkExecute(_arg0 invites.RevokeInviteLinkRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) SendInvites(_arg0 context.Context) invites.SendInvitesRequest {
	return withAPIService(invites.SendInvitesRequest{}, Client{})
}
func (Client) SendInvitesExecute(_arg0 invites.SendInvitesRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) SendMessage(_arg0 context.Context) messages.SendMessageRequest {
	return withAPIService(messages.SendMessageRequest{}, Client{})
}
func (Client) SendMessageExecute(_arg0 messages.SendMessageRequest) (*messages.SendMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) SetTypingStatus(_arg0 context.Context) users.SetTypingStatusRequest {
	return withAPIService(users.SetTypingStatusRequest{}, Client{})
}
func (Client) SetTypingStatusExecute(_arg0 users.SetTypingStatusRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) SetTypingStatusForMessageEdit(_arg0 context.Context, _arg1 int64) users.SetTypingStatusForMessageEditRequest {
	return withAPIService(users.SetTypingStatusForMessageEditRequest{}, Client{})
}
func (Client) SetTypingStatusForMessageEditExecute(_arg0 users.SetTypingStatusForMessageEditRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) Subscribe(ctx context.Context) channels.SubscribeRequest {
	return withContext(withAPIService(channels.SubscribeRequest{}, c), ctx)
}
func (c Client) SubscribeExecute(r channels.SubscribeRequest) (*channels.SubscribeResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationSubscribe, subscribeKey(r)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationSubscribe); err != nil {
		return nil, nil, err
	}

	subscriptions := requestSubscriptionsPtr(r)
	if subscriptions == nil {
		return nil, nil, fmt.Errorf("subscriptions is required")
	}
	userIDs := principalUserIDs(requestPrincipalsPtr(r, "principals"))
	resp := &channels.SubscribeResponse{
		Response:          successResponse(),
		Subscribed:        map[string][]string{},
		AlreadySubscribed: map[string][]string{},
	}
	for _, sub := range *subscriptions {
		channelID, ok := state.channelIDs[sub.Name]
		if !ok {
			channelID = state.nextChannelID
			state.nextChannelID++
			description := ""
			if sub.Description != nil {
				description = *sub.Description
			}
			state.channels[channelID] = channelState{
				channel: zulip.Channel{
					ChannelID:   channelID,
					Name:        sub.Name,
					Description: description,
				},
				subscribers: map[int64]bool{},
			}
			state.channelIDs[sub.Name] = channelID
		}
		channel := state.channels[channelID]
		for _, userID := range userIDs {
			key := fmt.Sprintf("%d", userID)
			if channel.subscribers[userID] {
				resp.AlreadySubscribed[key] = append(resp.AlreadySubscribed[key], sub.Name)
				continue
			}
			channel.subscribers[userID] = true
			resp.Subscribed[key] = append(resp.Subscribed[key], sub.Name)
		}
		state.channels[channelID] = channel
	}
	return resp, nil, nil
}
func (Client) TestNotify(_arg0 context.Context) mobile.TestNotifyRequest {
	return withAPIService(mobile.TestNotifyRequest{}, Client{})
}
func (Client) TestNotifyExecute(_arg0 mobile.TestNotifyRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) TestWelcomeBotCustomMessage(_arg0 context.Context) serverandorganizations.TestWelcomeBotCustomMessageRequest {
	return withAPIService(serverandorganizations.TestWelcomeBotCustomMessageRequest{}, Client{})
}
func (Client) TestWelcomeBotCustomMessageExecute(_arg0 serverandorganizations.TestWelcomeBotCustomMessageRequest) (*serverandorganizations.TestWelcomeBotCustomMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UnmuteUser(_arg0 context.Context, _arg1 int64) users.UnmuteUserRequest {
	return withAPIService(users.UnmuteUserRequest{}, Client{})
}
func (Client) UnmuteUserExecute(_arg0 users.UnmuteUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) Unsubscribe(ctx context.Context) channels.UnsubscribeRequest {
	return withContext(withAPIService(channels.UnsubscribeRequest{}, c), ctx)
}
func (c Client) UnsubscribeExecute(r channels.UnsubscribeRequest) (*channels.UnsubscribeResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationUnsubscribe, unsubscribeKey(r)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationUnsubscribe); err != nil {
		return nil, nil, err
	}

	subscriptions := requestSubscriptionNamesPtr(r)
	if subscriptions == nil {
		return nil, nil, fmt.Errorf("subscriptions is required")
	}
	userIDs := principalUserIDs(requestPrincipalsPtr(r, "principals"))
	resp := &channels.UnsubscribeResponse{Response: successResponse()}
	for _, name := range *subscriptions {
		channelID, ok := state.channelIDs[name]
		if !ok {
			resp.NotRemoved = append(resp.NotRemoved, name)
			continue
		}
		channel := state.channels[channelID]
		removed := false
		for _, userID := range userIDs {
			if !channel.subscribers[userID] {
				continue
			}
			delete(channel.subscribers, userID)
			removed = true
		}
		if removed {
			resp.Removed = append(resp.Removed, name)
		} else {
			resp.NotRemoved = append(resp.NotRemoved, name)
		}
		state.channels[channelID] = channel
	}
	return resp, nil, nil
}
func (Client) UpdateChannel(_arg0 context.Context, _arg1 int64) channels.UpdateChannelRequest {
	return withAPIService(channels.UpdateChannelRequest{}, Client{})
}
func (Client) UpdateChannelExecute(_arg0 channels.UpdateChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateChannelFolder(_arg0 context.Context, _arg1 int64) channels.UpdateChannelFolderRequest {
	return withAPIService(channels.UpdateChannelFolderRequest{}, Client{})
}
func (Client) UpdateChannelFolderExecute(_arg0 channels.UpdateChannelFolderRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateLinkifier(_arg0 context.Context, _arg1 int64) serverandorganizations.UpdateLinkifierRequest {
	return withAPIService(serverandorganizations.UpdateLinkifierRequest{}, Client{})
}
func (Client) UpdateLinkifierExecute(_arg0 serverandorganizations.UpdateLinkifierRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessage(_arg0 context.Context, _arg1 int64) messages.UpdateMessageRequest {
	return withAPIService(messages.UpdateMessageRequest{}, Client{})
}
func (Client) UpdateMessageExecute(_arg0 messages.UpdateMessageRequest) (*messages.UpdateMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessageFlags(_arg0 context.Context) messages.UpdateMessageFlagsRequest {
	return withAPIService(messages.UpdateMessageFlagsRequest{}, Client{})
}
func (Client) UpdateMessageFlagsExecute(_arg0 messages.UpdateMessageFlagsRequest) (*messages.UpdateMessageFlagsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessageFlagsForNarrow(_arg0 context.Context) messages.UpdateMessageFlagsForNarrowRequest {
	return withAPIService(messages.UpdateMessageFlagsForNarrowRequest{}, Client{})
}
func (Client) UpdateMessageFlagsForNarrowExecute(_arg0 messages.UpdateMessageFlagsForNarrowRequest) (*messages.UpdateMessageFlagsForNarrowResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdatePresence(_arg0 context.Context) users.UpdatePresenceRequest {
	return withAPIService(users.UpdatePresenceRequest{}, Client{})
}
func (Client) UpdatePresenceExecute(_arg0 users.UpdatePresenceRequest) (*users.UpdatePresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateRealmUserSettingsDefaults(_arg0 context.Context) serverandorganizations.UpdateRealmUserSettingsDefaultsRequest {
	return withAPIService(serverandorganizations.UpdateRealmUserSettingsDefaultsRequest{}, Client{})
}
func (Client) UpdateRealmUserSettingsDefaultsExecute(_arg0 serverandorganizations.UpdateRealmUserSettingsDefaultsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateScheduledMessage(_arg0 context.Context, _arg1 int64) scheduledmessages.UpdateScheduledMessageRequest {
	return withAPIService(scheduledmessages.UpdateScheduledMessageRequest{}, Client{})
}
func (Client) UpdateScheduledMessageExecute(_arg0 scheduledmessages.UpdateScheduledMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSettings(_arg0 context.Context) users.UpdateSettingsRequest {
	return withAPIService(users.UpdateSettingsRequest{}, Client{})
}
func (Client) UpdateSettingsExecute(_arg0 users.UpdateSettingsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateStatus(_arg0 context.Context) users.UpdateStatusRequest {
	return withAPIService(users.UpdateStatusRequest{}, Client{})
}
func (Client) UpdateStatusExecute(_arg0 users.UpdateStatusRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateStatusForUser(_arg0 context.Context, _arg1 int64) users.UpdateStatusForUserRequest {
	return withAPIService(users.UpdateStatusForUserRequest{}, Client{})
}
func (Client) UpdateStatusForUserExecute(_arg0 users.UpdateStatusForUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSubscriptionSettings(_arg0 context.Context) channels.UpdateSubscriptionSettingsRequest {
	return withAPIService(channels.UpdateSubscriptionSettingsRequest{}, Client{})
}
func (Client) UpdateSubscriptionSettingsExecute(_arg0 channels.UpdateSubscriptionSettingsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSubscriptions(_arg0 context.Context) channels.UpdateSubscriptionsRequest {
	return withAPIService(channels.UpdateSubscriptionsRequest{}, Client{})
}
func (Client) UpdateSubscriptionsExecute(_arg0 channels.UpdateSubscriptionsRequest) (*channels.UpdateSubscriptionsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUser(_arg0 context.Context, _arg1 int64) users.UpdateUserRequest {
	return withAPIService(users.UpdateUserRequest{}, Client{})
}
func (Client) UpdateUserByEmail(_arg0 context.Context, _arg1 string) users.UpdateUserByEmailRequest {
	return withAPIService(users.UpdateUserByEmailRequest{}, Client{})
}
func (Client) UpdateUserByEmailExecute(_arg0 users.UpdateUserByEmailRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUserExecute(_arg0 users.UpdateUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUserGroup(_arg0 context.Context, _arg1 int64) users.UpdateUserGroupRequest {
	return withAPIService(users.UpdateUserGroupRequest{}, Client{})
}
func (Client) UpdateUserGroupExecute(_arg0 users.UpdateUserGroupRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) UpdateUserGroupMembers(ctx context.Context, userGroupID int64) users.UpdateUserGroupMembersRequest {
	return withInt64Field(withContext(withAPIService(users.UpdateUserGroupMembersRequest{}, c), ctx), "userGroupID", userGroupID)
}
func (c Client) UpdateUserGroupMembersExecute(r users.UpdateUserGroupMembersRequest) (*zulip.Response, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationUpdateUserGroupMembers, updateUserGroupMembersKey(r)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationUpdateUserGroupMembers); err != nil {
		return nil, nil, err
	}

	id := requestInt64(r, "userGroupID")
	group, ok := state.userGroups[id]
	if !ok {
		return nil, nil, fmt.Errorf("user group %d not found", id)
	}
	members := map[int64]bool{}
	for _, memberID := range group.group.Members {
		members[memberID] = true
	}
	if del := requestInt64SlicePtr(r, "delete"); del != nil {
		for _, memberID := range *del {
			delete(members, memberID)
		}
	}
	if add := requestInt64SlicePtr(r, "add"); add != nil {
		for _, memberID := range *add {
			members[memberID] = true
		}
	}
	group.group.Members = group.group.Members[:0]
	for memberID := range members {
		group.group.Members = append(group.group.Members, memberID)
	}
	group.group.Members = sortedMemberIDs(group.group.Members)
	state.userGroups[id] = group
	resp := successResponse()
	return &resp, nil, nil
}
func (Client) UpdateUserGroupSubgroups(_arg0 context.Context, _arg1 int64) users.UpdateUserGroupSubgroupsRequest {
	return withAPIService(users.UpdateUserGroupSubgroupsRequest{}, Client{})
}
func (Client) UpdateUserGroupSubgroupsExecute(_arg0 users.UpdateUserGroupSubgroupsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUserTopic(_arg0 context.Context) channels.UpdateUserTopicRequest {
	return withAPIService(channels.UpdateUserTopicRequest{}, Client{})
}
func (Client) UpdateUserTopicExecute(_arg0 channels.UpdateUserTopicRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UploadCustomEmoji(_arg0 context.Context, _arg1 string) serverandorganizations.UploadCustomEmojiRequest {
	return withAPIService(serverandorganizations.UploadCustomEmojiRequest{}, Client{})
}
func (Client) UploadCustomEmojiExecute(_arg0 serverandorganizations.UploadCustomEmojiRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UploadFile(_arg0 context.Context) messages.UploadFileRequest {
	return withAPIService(messages.UploadFileRequest{}, Client{})
}
func (Client) UploadFileExecute(_arg0 messages.UploadFileRequest) (*messages.UploadFileResponse, *http.Response, error) {
	return nil, nil, nil
}
