// Code generated for tests; DO NOT EDIT.
package zulipmock

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"

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
)

type Client struct {
	state *state
}

type Operation string

const (
	OperationCreateChannel          Operation = "CreateChannel"
	OperationCreateChannelFolder    Operation = "CreateChannelFolder"
	OperationCreateUserGroup        Operation = "CreateUserGroup"
	OperationDeactivateUserGroup    Operation = "DeactivateUserGroup"
	OperationGetChannelByID         Operation = "GetChannelByID"
	OperationGetChannels            Operation = "GetChannels"
	OperationGetSubscribers         Operation = "GetSubscribers"
	OperationGetSubscriptions       Operation = "GetSubscriptions"
	OperationGetIsUserGroupMember   Operation = "GetIsUserGroupMember"
	OperationGetUserGroupMembers    Operation = "GetUserGroupMembers"
	OperationGetUserGroups          Operation = "GetUserGroups"
	OperationRegisterQueue          Operation = "RegisterQueue"
	OperationSubscribe              Operation = "Subscribe"
	OperationUnsubscribe            Operation = "Unsubscribe"
	OperationUpdateChannel          Operation = "UpdateChannel"
	OperationUpdateChannelFolder    Operation = "UpdateChannelFolder"
	OperationUpdateUserGroupMembers Operation = "UpdateUserGroupMembers"
)

type state struct {
	mu              sync.Mutex
	nextChannelID   int64
	nextFolderID    int64
	nextUserGroupID int64
	nextMessageID   int64
	ownUser         zulip.User
	users           map[int64]zulip.User
	lastMessage     *SentMessage
	typingStatuses  []TypingStatus
	channels        map[int64]channelState
	channelIDs      map[string]int64
	channelFolders  map[int64]zulip.ChannelFolder
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

func (s *state) renderedMentionUserIDLocked(content string) (int64, bool) {
	name, id, ok := splitUserMention(content)
	if !ok {
		return 0, false
	}
	if id != 0 {
		if _, ok := s.users[id]; ok {
			return id, true
		}
		return 0, false
	}
	for _, user := range s.users {
		if user.FullName == name {
			return user.UserID, true
		}
	}
	return 0, false
}

func (s *state) renderedMentionChannelIDLocked(content string) (int64, bool) {
	name, ok := splitChannelMention(content)
	if !ok {
		return 0, false
	}
	id, ok := s.channelIDs[name]
	return id, ok
}

func splitUserMention(content string) (string, int64, bool) {
	content = strings.TrimSpace(content)
	prefix := "@**"
	if strings.HasPrefix(content, "@_**") {
		prefix = "@_**"
	}
	if !strings.HasPrefix(content, prefix) || !strings.HasSuffix(content, "**") {
		return "", 0, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(content, prefix), "**")
	name, rawID, hasID := strings.Cut(body, "|")
	if !hasID {
		return name, 0, name != ""
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name, id, name != ""
}

func splitChannelMention(content string) (string, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "#**") || !strings.HasSuffix(content, "**") {
		return "", false
	}
	name := strings.TrimSuffix(strings.TrimPrefix(content, "#**"), "**")
	return name, name != ""
}

type SentMessage struct {
	Recipient     zulip.Recipient
	RecipientType *zulip.RecipientType
	Content       string
	Topic         string
}

type TypingStatus struct {
	Recipient     zulip.Recipient
	RecipientType *zulip.RecipientType
	Op            zulip.TypingStatusOp
}

var _ client.Client = Client{}

func NewClient() Client {
	return Client{state: &state{
		nextChannelID:   1,
		nextFolderID:    1,
		nextUserGroupID: 100,
		nextMessageID:   1,
		ownUser: zulip.User{
			UserID:   1,
			Email:    "mock-bot@example.com",
			FullName: "Mock Bot",
			IsBot:    true,
		},
		users: map[int64]zulip.User{
			1: {
				UserID:   1,
				Email:    "mock-bot@example.com",
				FullName: "Mock Bot",
				IsBot:    true,
			},
		},
		channels:       map[int64]channelState{},
		channelIDs:     map[string]int64{},
		channelFolders: map[int64]zulip.ChannelFolder{},
		userGroups: map[int64]userGroupState{
			42: {group: zulip.UserGroup{
				ID:            42,
				Name:          "role:administrators",
				Description:   "Administrators of this organization, including owners",
				IsSystemGroup: true,
			}},
		},
		failures: map[Operation][]error{},
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

func (c Client) SetOwnUser(user zulip.User) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	state.ownUser = user
	if user.UserID != 0 {
		state.users[user.UserID] = user
	}
}

func (c Client) AddUser(user zulip.User) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	if user.UserID == 0 {
		return
	}
	state.users[user.UserID] = user
}

func (c Client) LastSentMessage() *SentMessage {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.lastMessage == nil {
		return nil
	}
	copy := *state.lastMessage
	return &copy
}

func (c Client) TypingStatuses() []TypingStatus {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	statuses := make([]TypingStatus, len(state.typingStatuses))
	copy(statuses, state.typingStatuses)
	return statuses
}

func (c Client) DeleteUserGroupForTest(userGroupID int64) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	delete(state.userGroups, userGroupID)
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

func requestInt64Ptr[T any](request T, name string) *int64 {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*int64)(unsafe.Pointer(field.Pointer()))
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

func requestBoolPtr[T any](request T, name string) *bool {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*bool)(unsafe.Pointer(field.Pointer()))
}

func requestInt64SlicePtr[T any](request T, name string) *[]int64 {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*[]int64)(unsafe.Pointer(field.Pointer()))
}

func requestRecipientPtr[T any](request T, name string) *zulip.Recipient {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.Recipient)(unsafe.Pointer(field.Pointer()))
}

func requestRecipientTypePtr[T any](request T, name string) *zulip.RecipientType {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.RecipientType)(unsafe.Pointer(field.Pointer()))
}

func requestTypingStatusOpPtr[T any](request T, name string) *zulip.TypingStatusOp {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.TypingStatusOp)(unsafe.Pointer(field.Pointer()))
}

func requestClient[T any](request T) Client {
	v := reflect.ValueOf(&request).Elem()
	field := v.FieldByName("apiService")
	if !field.IsValid() || !field.CanAddr() {
		return Client{}
	}
	value := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
	service, ok := value.(Client)
	if ok {
		return service
	}
	return Client{}
}

func requestPrincipalsPtr[T any](request T, name string) *zulip.Principals {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.Principals)(unsafe.Pointer(field.Pointer()))
}

func requestGroupSettingValuePtr[T any](request T, name string) *zulip.GroupSettingValue {
	v := reflect.ValueOf(request)
	field := v.FieldByName(name)
	if field.IsNil() {
		return nil
	}
	return (*zulip.GroupSettingValue)(unsafe.Pointer(field.Pointer()))
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
func (Client) AddAlertWords(_ context.Context) users.AddAlertWordsRequest {
	return withAPIService(users.AddAlertWordsRequest{}, Client{})
}
func (Client) AddAlertWordsExecute(_ users.AddAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddApnsToken(_ context.Context) users.AddApnsTokenRequest {
	return withAPIService(users.AddApnsTokenRequest{}, Client{})
}
func (Client) AddApnsTokenExecute(_ users.AddApnsTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddCodePlayground(_ context.Context) serverandorganizations.AddCodePlaygroundRequest {
	return withAPIService(serverandorganizations.AddCodePlaygroundRequest{}, Client{})
}
func (Client) AddCodePlaygroundExecute(_ serverandorganizations.AddCodePlaygroundRequest) (*serverandorganizations.AddCodePlaygroundResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddDefaultChannel(_ context.Context) channels.AddDefaultChannelRequest {
	return withAPIService(channels.AddDefaultChannelRequest{}, Client{})
}
func (Client) AddDefaultChannelExecute(_ channels.AddDefaultChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddFcmToken(_ context.Context) users.AddFcmTokenRequest {
	return withAPIService(users.AddFcmTokenRequest{}, Client{})
}
func (Client) AddFcmTokenExecute(_ users.AddFcmTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddLinkifier(_ context.Context) serverandorganizations.AddLinkifierRequest {
	return withAPIService(serverandorganizations.AddLinkifierRequest{}, Client{})
}
func (Client) AddLinkifierExecute(_ serverandorganizations.AddLinkifierRequest) (*serverandorganizations.AddLinkifierResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddNavigationView(_ context.Context) navigationviews.AddNavigationViewRequest {
	return withAPIService(navigationviews.AddNavigationViewRequest{}, Client{})
}
func (Client) AddNavigationViewExecute(_ navigationviews.AddNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) AddReaction(_ context.Context, _arg1 int64) messages.AddReactionRequest {
	return withAPIService(messages.AddReactionRequest{}, Client{})
}
func (Client) AddReactionExecute(_ messages.AddReactionRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ArchiveChannel(_ context.Context, _arg1 int64) channels.ArchiveChannelRequest {
	return withAPIService(channels.ArchiveChannelRequest{}, Client{})
}
func (Client) ArchiveChannelExecute(_ channels.ArchiveChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CheckMessagesMatchNarrow(_ context.Context) messages.CheckMessagesMatchNarrowRequest {
	return withAPIService(messages.CheckMessagesMatchNarrowRequest{}, Client{})
}
func (Client) CheckMessagesMatchNarrowExecute(_ messages.CheckMessagesMatchNarrowRequest) (*messages.CheckMessagesMatchNarrowResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateBigBlueButtonVideoCall(_ context.Context) channels.CreateBigBlueButtonVideoCallRequest {
	return withAPIService(channels.CreateBigBlueButtonVideoCallRequest{}, Client{})
}
func (Client) CreateBigBlueButtonVideoCallExecute(_ channels.CreateBigBlueButtonVideoCallRequest) (*channels.CreateBigBlueButtonVideoCallResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) CreateChannel(ctx context.Context) channels.CreateChannelRequest {
	return withContext(withAPIService(channels.CreateChannelRequest{}, c), ctx)
}
func (c Client) CreateChannelExecute(r channels.CreateChannelRequest) (*channels.CreateChannelResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationCreateChannel, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationCreateChannel); err != nil {
		return nil, nil, err
	}

	name := ""
	if v := requestStringPtr(r, "name"); v != nil {
		name = *v
	}
	if name == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	if _, ok := state.channelIDs[name]; ok {
		return nil, nil, zulip.CodedError{
			Response: zulip.Response{
				Result: zulip.ResponseError,
				Msg:    fmt.Sprintf("Channel %q already exists", name),
			},
			Code: "CHANNEL_ALREADY_EXISTS",
		}
	}
	description := ""
	if v := requestStringPtr(r, "description"); v != nil {
		description = *v
	}

	channelID := state.nextChannelID
	state.nextChannelID++
	state.channels[channelID] = channelState{
		channel: zulip.Channel{
			ChannelID:   channelID,
			Name:        name,
			Description: description,
		},
		subscribers: map[int64]bool{},
	}
	state.channelIDs[name] = channelID

	if subscribers := requestInt64SlicePtr(r, "subscribers"); subscribers != nil {
		channel := state.channels[channelID]
		for _, userID := range *subscribers {
			channel.subscribers[userID] = true
		}
		state.channels[channelID] = channel
	}

	return &channels.CreateChannelResponse{
		Response: successResponse(),
		ID:       channelID,
	}, nil, nil
}
func (c Client) CreateChannelFolder(ctx context.Context) channels.CreateChannelFolderRequest {
	return withContext(withAPIService(channels.CreateChannelFolderRequest{}, c), ctx)
}
func (c Client) CreateChannelFolderExecute(r channels.CreateChannelFolderRequest) (*channels.CreateChannelFolderResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationCreateChannelFolder, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationCreateChannelFolder); err != nil {
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
	}
	id := state.nextFolderID
	state.nextFolderID++
	state.channelFolders[id] = zulip.ChannelFolder{
		ID:          id,
		Name:        name,
		Description: description,
	}
	return &channels.CreateChannelFolderResponse{
		Response:        successResponse(),
		ChannelFolderID: id,
	}, nil, nil
}
func (Client) CreateCustomProfileField(_ context.Context) serverandorganizations.CreateCustomProfileFieldRequest {
	return withAPIService(serverandorganizations.CreateCustomProfileFieldRequest{}, Client{})
}
func (Client) CreateCustomProfileFieldExecute(_ serverandorganizations.CreateCustomProfileFieldRequest) (*serverandorganizations.CreateCustomProfileFieldResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateDrafts(_ context.Context) drafts.CreateDraftsRequest {
	return withAPIService(drafts.CreateDraftsRequest{}, Client{})
}
func (Client) CreateDraftsExecute(_ drafts.CreateDraftsRequest) (*drafts.CreateDraftsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateInviteLink(_ context.Context) invites.CreateInviteLinkRequest {
	return withAPIService(invites.CreateInviteLinkRequest{}, Client{})
}
func (Client) CreateInviteLinkExecute(_ invites.CreateInviteLinkRequest) (*invites.CreateInviteLinkResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateMessageReminder(_ context.Context) reminders.CreateMessageReminderRequest {
	return withAPIService(reminders.CreateMessageReminderRequest{}, Client{})
}
func (Client) CreateMessageReminderExecute(_ reminders.CreateMessageReminderRequest) (*reminders.CreateMessageReminderResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateSavedSnippet(_ context.Context) drafts.CreateSavedSnippetRequest {
	return withAPIService(drafts.CreateSavedSnippetRequest{}, Client{})
}
func (Client) CreateSavedSnippetExecute(_ drafts.CreateSavedSnippetRequest) (*drafts.CreateSavedSnippetResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateScheduledMessage(_ context.Context) scheduledmessages.CreateScheduledMessageRequest {
	return withAPIService(scheduledmessages.CreateScheduledMessageRequest{}, Client{})
}
func (Client) CreateScheduledMessageExecute(_ scheduledmessages.CreateScheduledMessageRequest) (*scheduledmessages.CreateScheduledMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) CreateUser(_ context.Context) users.CreateUserRequest {
	return withAPIService(users.CreateUserRequest{}, Client{})
}
func (Client) CreateUserExecute(_ users.CreateUserRequest) (*users.CreateUserResponse, *http.Response, error) {
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
	for _, group := range state.userGroups {
		if group.group.Name == name {
			return nil, nil, zulip.CodedError{
				Response: zulip.Response{
					Result: zulip.ResponseError,
					Msg:    fmt.Sprintf("User group %q already exists.", name),
				},
				Code: "BAD_REQUEST",
			}
		}
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
		ID:                    id,
		Name:                  name,
		Description:           description,
		Members:               members,
		DirectSubgroupIDs:     subgroups,
		CanAddMembersGroup:    requestGroupSettingValue(r, "canAddMembersGroup"),
		CanJoinGroup:          requestGroupSettingValue(r, "canJoinGroup"),
		CanLeaveGroup:         requestGroupSettingValue(r, "canLeaveGroup"),
		CanManageGroup:        requestGroupSettingValue(r, "canManageGroup"),
		CanMentionGroup:       requestGroupSettingValue(r, "canMentionGroup"),
		CanRemoveMembersGroup: requestGroupSettingValue(r, "canRemoveMembersGroup"),
	}}
	state.users[id] = zulip.User{
		UserID:   id,
		FullName: name,
	}

	return &users.CreateUserGroupResponse{Response: successResponse(), GroupID: id}, nil, nil
}

func requestGroupSettingValue(r users.CreateUserGroupRequest, name string) zulip.GroupSettingValue {
	if value := requestGroupSettingValuePtr(r, name); value != nil {
		return *value
	}
	return zulip.GroupSettingValue{}
}

func (Client) DeactivateCustomEmoji(_ context.Context, _arg1 string) serverandorganizations.DeactivateCustomEmojiRequest {
	return withAPIService(serverandorganizations.DeactivateCustomEmojiRequest{}, Client{})
}
func (Client) DeactivateCustomEmojiExecute(_ serverandorganizations.DeactivateCustomEmojiRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeactivateOwnUser(_ context.Context) users.DeactivateOwnUserRequest {
	return withAPIService(users.DeactivateOwnUserRequest{}, Client{})
}
func (Client) DeactivateOwnUserExecute(_ users.DeactivateOwnUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeactivateUser(_ context.Context, _arg1 int64) users.DeactivateUserRequest {
	return withAPIService(users.DeactivateUserRequest{}, Client{})
}
func (Client) DeactivateUserExecute(_ users.DeactivateUserRequest) (*zulip.Response, *http.Response, error) {
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
func (Client) DeleteDraft(_ context.Context, _arg1 int64) drafts.DeleteDraftRequest {
	return withAPIService(drafts.DeleteDraftRequest{}, Client{})
}
func (Client) DeleteDraftExecute(_ drafts.DeleteDraftRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteMessage(_ context.Context, _arg1 int64) messages.DeleteMessageRequest {
	return withAPIService(messages.DeleteMessageRequest{}, Client{})
}
func (Client) DeleteMessageExecute(_ messages.DeleteMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) DeleteQueue(ctx context.Context) realtimeevents.DeleteQueueRequest {
	return withContext(withAPIService(realtimeevents.DeleteQueueRequest{}, c), ctx)
}
func (Client) DeleteQueueExecute(_ realtimeevents.DeleteQueueRequest) (*zulip.Response, *http.Response, error) {
	resp := successResponse()
	return &resp, nil, nil
}
func (Client) DeleteReminder(_ context.Context, _arg1 int64) reminders.DeleteReminderRequest {
	return withAPIService(reminders.DeleteReminderRequest{}, Client{})
}
func (Client) DeleteReminderExecute(_ reminders.DeleteReminderRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteSavedSnippet(_ context.Context, _arg1 int64) drafts.DeleteSavedSnippetRequest {
	return withAPIService(drafts.DeleteSavedSnippetRequest{}, Client{})
}
func (Client) DeleteSavedSnippetExecute(_ drafts.DeleteSavedSnippetRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteScheduledMessage(_ context.Context, _arg1 int64) scheduledmessages.DeleteScheduledMessageRequest {
	return withAPIService(scheduledmessages.DeleteScheduledMessageRequest{}, Client{})
}
func (Client) DeleteScheduledMessageExecute(_ scheduledmessages.DeleteScheduledMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DeleteTopic(_ context.Context, _arg1 int64) channels.DeleteTopicRequest {
	return withAPIService(channels.DeleteTopicRequest{}, Client{})
}
func (Client) DeleteTopicExecute(_ channels.DeleteTopicRequest) (*channels.MarkAllAsReadResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) DevFetchAPIKey(_ context.Context) authentication.DevFetchAPIKeyRequest {
	return withAPIService(authentication.DevFetchAPIKeyRequest{}, Client{})
}
func (Client) DevFetchAPIKeyExecute(_ authentication.DevFetchAPIKeyRequest) (*authentication.APIKeyResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) E2eeTestNotify(_ context.Context) mobile.E2eeTestNotifyRequest {
	return withAPIService(mobile.E2eeTestNotifyRequest{}, Client{})
}
func (Client) E2eeTestNotifyExecute(_ mobile.E2eeTestNotifyRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditDraft(_ context.Context, _arg1 int64) drafts.EditDraftRequest {
	return withAPIService(drafts.EditDraftRequest{}, Client{})
}
func (Client) EditDraftExecute(_ drafts.EditDraftRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditNavigationView(_ context.Context, _arg1 string) navigationviews.EditNavigationViewRequest {
	return withAPIService(navigationviews.EditNavigationViewRequest{}, Client{})
}
func (Client) EditNavigationViewExecute(_ navigationviews.EditNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) EditSavedSnippet(_ context.Context, _arg1 int64) drafts.EditSavedSnippetRequest {
	return withAPIService(drafts.EditSavedSnippetRequest{}, Client{})
}
func (Client) EditSavedSnippetExecute(_ drafts.EditSavedSnippetRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ExportRealm(_ context.Context) serverandorganizations.ExportRealmRequest {
	return withAPIService(serverandorganizations.ExportRealmRequest{}, Client{})
}
func (Client) ExportRealmExecute(_ serverandorganizations.ExportRealmRequest) (*serverandorganizations.ExportRealmResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) FetchAPIKey(_ context.Context) authentication.FetchAPIKeyRequest {
	return withAPIService(authentication.FetchAPIKeyRequest{}, Client{})
}
func (Client) FetchAPIKeyExecute(_ authentication.FetchAPIKeyRequest) (*authentication.APIKeyResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetAlertWords(_ context.Context) users.GetAlertWordsRequest {
	return withAPIService(users.GetAlertWordsRequest{}, Client{})
}
func (Client) GetAlertWordsExecute(_ users.GetAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetAttachments(_ context.Context) users.GetAttachmentsRequest {
	return withAPIService(users.GetAttachmentsRequest{}, Client{})
}
func (Client) GetAttachmentsExecute(_ users.GetAttachmentsRequest) (*users.GetAttachmentsResponse, *http.Response, error) {
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
func (Client) GetChannelEmailAddress(_ context.Context, _arg1 int64) channels.GetChannelEmailAddressRequest {
	return withAPIService(channels.GetChannelEmailAddressRequest{}, Client{})
}
func (Client) GetChannelEmailAddressExecute(_ channels.GetChannelEmailAddressRequest) (*channels.GetChannelEmailAddressResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetChannelFolders(ctx context.Context) channels.GetChannelFoldersRequest {
	return withContext(withAPIService(channels.GetChannelFoldersRequest{}, c), ctx)
}
func (c Client) GetChannelFoldersExecute(_ channels.GetChannelFoldersRequest) (*channels.GetChannelFoldersResponse, *http.Response, error) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	folders := make([]zulip.ChannelFolder, 0, len(state.channelFolders))
	for _, folder := range state.channelFolders {
		folders = append(folders, folder)
	}
	sort.Slice(folders, func(i, j int) bool { return folders[i].ID < folders[j].ID })
	return &channels.GetChannelFoldersResponse{
		Response:       successResponse(),
		ChannelFolders: folders,
	}, nil, nil
}
func (c Client) GetChannelID(ctx context.Context) channels.GetChannelIDRequest {
	return withContext(withAPIService(channels.GetChannelIDRequest{}, c), ctx)
}
func (Client) GetChannelIDExecute(r channels.GetChannelIDRequest) (*channels.GetChannelIDResponse, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	name := ""
	if value := requestStringPtr(r, "channel"); value != nil {
		name = *value
	}
	id, ok := state.channelIDs[name]
	if !ok {
		return nil, nil, fmt.Errorf("channel %q not found", name)
	}
	return &channels.GetChannelIDResponse{Response: successResponse(), ChannelID: id}, nil, nil
}
func (Client) GetChannelTopics(_ context.Context, _arg1 int64) channels.GetChannelTopicsRequest {
	return withAPIService(channels.GetChannelTopicsRequest{}, Client{})
}
func (Client) GetChannelTopicsExecute(_ channels.GetChannelTopicsRequest) (*channels.GetChannelTopicsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetChannels(ctx context.Context) channels.GetChannelsRequest {
	return withContext(withAPIService(channels.GetChannelsRequest{}, c), ctx)
}
func (c Client) GetChannelsExecute(r channels.GetChannelsRequest) (*channels.GetChannelsResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetChannels, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetChannels); err != nil {
		return nil, nil, err
	}

	list := make([]zulip.Channel, 0, len(state.channels))
	for _, channel := range state.channels {
		list = append(list, channel.channel)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ChannelID < list[j].ChannelID })
	return &channels.GetChannelsResponse{Response: successResponse(), Channels: list}, nil, nil
}
func (Client) GetCustomEmoji(_ context.Context) serverandorganizations.GetCustomEmojiRequest {
	return withAPIService(serverandorganizations.GetCustomEmojiRequest{}, Client{})
}
func (Client) GetCustomEmojiExecute(_ serverandorganizations.GetCustomEmojiRequest) (*serverandorganizations.GetCustomEmojiResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetCustomProfileFields(_ context.Context) serverandorganizations.GetCustomProfileFieldsRequest {
	return withAPIService(serverandorganizations.GetCustomProfileFieldsRequest{}, Client{})
}
func (Client) GetCustomProfileFieldsExecute(_ serverandorganizations.GetCustomProfileFieldsRequest) (*serverandorganizations.GetCustomProfileFieldsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetDrafts(_ context.Context) drafts.GetDraftsRequest {
	return withAPIService(drafts.GetDraftsRequest{}, Client{})
}
func (Client) GetDraftsExecute(_ drafts.GetDraftsRequest) (*drafts.GetDraftsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetEvents(ctx context.Context) realtimeevents.GetEventsRequest {
	return withContext(withAPIService(realtimeevents.GetEventsRequest{}, c), ctx)
}
func (Client) GetEventsExecute(r realtimeevents.GetEventsRequest) (*realtimeevents.GetEventsResponse, *http.Response, error) {
	if dontBlock := requestBoolPtr(r, "dontBlock"); dontBlock == nil || !*dontBlock {
		<-requestContext(r).Done()
		return nil, nil, requestContext(r).Err()
	}
	return &realtimeevents.GetEventsResponse{Response: successResponse()}, nil, nil
}
func (Client) GetFileTemporaryURL(_ context.Context, _arg1 int64, _arg2 string) messages.GetFileTemporaryURLRequest {
	return withAPIService(messages.GetFileTemporaryURLRequest{}, Client{})
}
func (Client) GetFileTemporaryURLExecute(_ messages.GetFileTemporaryURLRequest) (*messages.GetFileTemporaryURLResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetInvites(_ context.Context) invites.GetInvitesRequest {
	return withAPIService(invites.GetInvitesRequest{}, Client{})
}
func (Client) GetInvitesExecute(_ invites.GetInvitesRequest) (*invites.GetInvitesResponse, *http.Response, error) {
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
func (Client) GetLinkifiers(_ context.Context) serverandorganizations.GetLinkifiersRequest {
	return withAPIService(serverandorganizations.GetLinkifiersRequest{}, Client{})
}
func (Client) GetLinkifiersExecute(_ serverandorganizations.GetLinkifiersRequest) (*serverandorganizations.GetLinkifiersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessage(_ context.Context, _arg1 int64) messages.GetMessageRequest {
	return withAPIService(messages.GetMessageRequest{}, Client{})
}
func (Client) GetMessageExecute(_ messages.GetMessageRequest) (*messages.GetMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessageHistory(_ context.Context, _arg1 int64) messages.GetMessageHistoryRequest {
	return withAPIService(messages.GetMessageHistoryRequest{}, Client{})
}
func (Client) GetMessageHistoryExecute(_ messages.GetMessageHistoryRequest) (*messages.GetMessageHistoryResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetMessages(_ context.Context) messages.GetMessagesRequest {
	return withAPIService(messages.GetMessagesRequest{}, Client{})
}
func (Client) GetMessagesExecute(_ messages.GetMessagesRequest) (*messages.GetMessagesResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetNavigationViews(_ context.Context) navigationviews.GetNavigationViewsRequest {
	return withAPIService(navigationviews.GetNavigationViewsRequest{}, Client{})
}
func (Client) GetNavigationViewsExecute(_ navigationviews.GetNavigationViewsRequest) (*navigationviews.GetNavigationViewsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetOwnUser(ctx context.Context) users.GetOwnUserRequest {
	return withContext(withAPIService(users.GetOwnUserRequest{}, c), ctx)
}
func (Client) GetOwnUserExecute(r users.GetOwnUserRequest) (*users.GetOwnUserResponse, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	if state.ownUser.UserID == 0 {
		return nil, nil, fmt.Errorf("own user not configured")
	}
	return &users.GetOwnUserResponse{Response: successResponse(), User: state.ownUser}, nil, nil
}
func (Client) GetPresence(_ context.Context) serverandorganizations.GetPresenceRequest {
	return withAPIService(serverandorganizations.GetPresenceRequest{}, Client{})
}
func (Client) GetPresenceExecute(_ serverandorganizations.GetPresenceRequest) (*serverandorganizations.GetPresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetReadReceipts(_ context.Context, _arg1 int64) messages.GetReadReceiptsRequest {
	return withAPIService(messages.GetReadReceiptsRequest{}, Client{})
}
func (Client) GetReadReceiptsExecute(_ messages.GetReadReceiptsRequest) (*messages.GetReadReceiptsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetRealmExportConsents(_ context.Context) serverandorganizations.GetRealmExportConsentsRequest {
	return withAPIService(serverandorganizations.GetRealmExportConsentsRequest{}, Client{})
}
func (Client) GetRealmExportConsentsExecute(_ serverandorganizations.GetRealmExportConsentsRequest) (*serverandorganizations.GetRealmExportConsentsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetRealmExports(_ context.Context) serverandorganizations.GetRealmExportsRequest {
	return withAPIService(serverandorganizations.GetRealmExportsRequest{}, Client{})
}
func (Client) GetRealmExportsExecute(_ serverandorganizations.GetRealmExportsRequest) (*serverandorganizations.GetRealmExportsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetReminders(_ context.Context) reminders.GetRemindersRequest {
	return withAPIService(reminders.GetRemindersRequest{}, Client{})
}
func (Client) GetRemindersExecute(_ reminders.GetRemindersRequest) (*reminders.GetRemindersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetSavedSnippets(_ context.Context) drafts.GetSavedSnippetsRequest {
	return withAPIService(drafts.GetSavedSnippetsRequest{}, Client{})
}
func (Client) GetSavedSnippetsExecute(_ drafts.GetSavedSnippetsRequest) (*drafts.GetSavedSnippetsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetScheduledMessages(_ context.Context) scheduledmessages.GetScheduledMessagesRequest {
	return withAPIService(scheduledmessages.GetScheduledMessagesRequest{}, Client{})
}
func (Client) GetScheduledMessagesExecute(_ scheduledmessages.GetScheduledMessagesRequest) (*scheduledmessages.GetScheduledMessagesResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetServerSettings(_ context.Context) serverandorganizations.GetServerSettingsRequest {
	return withAPIService(serverandorganizations.GetServerSettingsRequest{}, Client{})
}
func (Client) GetServerSettingsExecute(_ serverandorganizations.GetServerSettingsRequest) (*serverandorganizations.GetServerSettingsResponse, *http.Response, error) {
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
func (Client) GetSubscriptionStatus(_ context.Context, _arg1 int64, _arg2 int64) channels.GetSubscriptionStatusRequest {
	return withAPIService(channels.GetSubscriptionStatusRequest{}, Client{})
}
func (Client) GetSubscriptionStatusExecute(_ channels.GetSubscriptionStatusRequest) (*channels.GetSubscriptionStatusResponse, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) GetSubscriptions(ctx context.Context) channels.GetSubscriptionsRequest {
	return withContext(withAPIService(channels.GetSubscriptionsRequest{}, c), ctx)
}
func (c Client) GetSubscriptionsExecute(r channels.GetSubscriptionsRequest) (*channels.GetSubscriptionsResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationGetSubscriptions, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationGetSubscriptions); err != nil {
		return nil, nil, err
	}

	subscriptions := make([]zulip.Subscription, 0, len(state.channels))
	for _, channel := range state.channels {
		if !channel.subscribers[0] {
			continue
		}
		subscriptions = append(subscriptions, zulip.Subscription{Channel: channel.channel})
	}
	sort.Slice(subscriptions, func(i, j int) bool {
		return subscriptions[i].ChannelID < subscriptions[j].ChannelID
	})
	return &channels.GetSubscriptionsResponse{
		Response:      successResponse(),
		Subscriptions: subscriptions,
	}, nil, nil
}
func (c Client) GetUser(ctx context.Context, userID int64) users.GetUserRequest {
	return withInt64Field(withContext(withAPIService(users.GetUserRequest{}, c), ctx), "userID", userID)
}
func (Client) GetUserByEmail(_ context.Context, _arg1 string) users.GetUserByEmailRequest {
	return withAPIService(users.GetUserByEmailRequest{}, Client{})
}
func (Client) GetUserByEmailExecute(_ users.GetUserByEmailRequest) (*users.GetUserResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUserExecute(r users.GetUserRequest) (*users.GetUserResponse, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	userID := requestInt64(r, "userID")
	user, ok := state.users[userID]
	if !ok {
		if group, groupOK := state.userGroups[userID]; groupOK {
			user = zulip.User{UserID: userID, FullName: group.group.Name}
			ok = true
		}
	}
	if !ok {
		return nil, nil, fmt.Errorf("user %d not found", userID)
	}
	return &users.GetUserResponse{Response: successResponse(), User: user}, nil, nil
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
func (Client) GetUserGroupSubgroups(_ context.Context, _arg1 int64) users.GetUserGroupSubgroupsRequest {
	return withAPIService(users.GetUserGroupSubgroupsRequest{}, Client{})
}
func (Client) GetUserGroupSubgroupsExecute(_ users.GetUserGroupSubgroupsRequest) (*users.GetUserGroupSubgroupsResponse, *http.Response, error) {
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
func (Client) GetUserPresence(_ context.Context, _arg1 string) users.GetUserPresenceRequest {
	return withAPIService(users.GetUserPresenceRequest{}, Client{})
}
func (Client) GetUserPresenceExecute(_ users.GetUserPresenceRequest) (*users.GetUserPresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUserStatus(_ context.Context, _arg1 int64) users.GetUserStatusRequest {
	return withAPIService(users.GetUserStatusRequest{}, Client{})
}
func (Client) GetUserStatusExecute(_ users.GetUserStatusRequest) (*users.GetUserStatusResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) GetUsers(_ context.Context) users.GetUsersRequest {
	return withAPIService(users.GetUsersRequest{}, Client{})
}
func (Client) GetUsersExecute(_ users.GetUsersRequest) (*users.GetUsersResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkAllAsRead(_ context.Context) messages.MarkAllAsReadRequest {
	return withAPIService(messages.MarkAllAsReadRequest{}, Client{})
}
func (Client) MarkAllAsReadExecute(_ messages.MarkAllAsReadRequest) (*messages.MarkAllAsReadResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkChannelAsRead(_ context.Context) messages.MarkChannelAsReadRequest {
	return withAPIService(messages.MarkChannelAsReadRequest{}, Client{})
}
func (Client) MarkChannelAsReadExecute(_ messages.MarkChannelAsReadRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MarkTopicAsRead(_ context.Context) messages.MarkTopicAsReadRequest {
	return withAPIService(messages.MarkTopicAsReadRequest{}, Client{})
}
func (Client) MarkTopicAsReadExecute(_ messages.MarkTopicAsReadRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MuteTopic(_ context.Context) channels.MuteTopicRequest {
	return withAPIService(channels.MuteTopicRequest{}, Client{})
}
func (Client) MuteTopicExecute(_ channels.MuteTopicRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) MuteUser(_ context.Context, _arg1 int64) users.MuteUserRequest {
	return withAPIService(users.MuteUserRequest{}, Client{})
}
func (Client) MuteUserExecute(_ users.MuteUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) PatchChannelFolders(_ context.Context) channels.PatchChannelFoldersRequest {
	return withAPIService(channels.PatchChannelFoldersRequest{}, Client{})
}
func (Client) PatchChannelFoldersExecute(_ channels.PatchChannelFoldersRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReactivateUser(_ context.Context, _arg1 int64) users.ReactivateUserRequest {
	return withAPIService(users.ReactivateUserRequest{}, Client{})
}
func (Client) ReactivateUserExecute(_ users.ReactivateUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RegisterPushDevice(_ context.Context) mobile.RegisterPushDeviceRequest {
	return withAPIService(mobile.RegisterPushDeviceRequest{}, Client{})
}
func (Client) RegisterPushDeviceExecute(_ mobile.RegisterPushDeviceRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) RegisterQueue(ctx context.Context) realtimeevents.RegisterQueueRequest {
	return withContext(withAPIService(realtimeevents.RegisterQueueRequest{}, c), ctx)
}
func (c Client) RegisterQueueExecute(r realtimeevents.RegisterQueueRequest) (*realtimeevents.RegisterQueueResponse, *http.Response, error) {
	state := c.ensureState()
	if err := state.waitForTurn(requestContext(r), OperationRegisterQueue, ""); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()

	if err := state.failLocked(OperationRegisterQueue); err != nil {
		return nil, nil, err
	}

	queueID := "mock-channelgroup-queue"
	return &realtimeevents.RegisterQueueResponse{
		Response:    successResponse(),
		QueueID:     &queueID,
		LastEventID: 0,
	}, nil, nil
}
func (Client) RemoveAlertWords(_ context.Context) users.RemoveAlertWordsRequest {
	return withAPIService(users.RemoveAlertWordsRequest{}, Client{})
}
func (Client) RemoveAlertWordsExecute(_ users.RemoveAlertWordsRequest) (*users.AlertWordsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveApnsToken(_ context.Context) users.RemoveApnsTokenRequest {
	return withAPIService(users.RemoveApnsTokenRequest{}, Client{})
}
func (Client) RemoveApnsTokenExecute(_ users.RemoveApnsTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveAttachment(_ context.Context, _arg1 int64) users.RemoveAttachmentRequest {
	return withAPIService(users.RemoveAttachmentRequest{}, Client{})
}
func (Client) RemoveAttachmentExecute(_ users.RemoveAttachmentRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveCodePlayground(_ context.Context, _arg1 int64) serverandorganizations.RemoveCodePlaygroundRequest {
	return withAPIService(serverandorganizations.RemoveCodePlaygroundRequest{}, Client{})
}
func (Client) RemoveCodePlaygroundExecute(_ serverandorganizations.RemoveCodePlaygroundRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveDefaultChannel(_ context.Context) channels.RemoveDefaultChannelRequest {
	return withAPIService(channels.RemoveDefaultChannelRequest{}, Client{})
}
func (Client) RemoveDefaultChannelExecute(_ channels.RemoveDefaultChannelRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveFcmToken(_ context.Context) users.RemoveFcmTokenRequest {
	return withAPIService(users.RemoveFcmTokenRequest{}, Client{})
}
func (Client) RemoveFcmTokenExecute(_ users.RemoveFcmTokenRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveLinkifier(_ context.Context, _arg1 int64) serverandorganizations.RemoveLinkifierRequest {
	return withAPIService(serverandorganizations.RemoveLinkifierRequest{}, Client{})
}
func (Client) RemoveLinkifierExecute(_ serverandorganizations.RemoveLinkifierRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveNavigationView(_ context.Context, _arg1 string) navigationviews.RemoveNavigationViewRequest {
	return withAPIService(navigationviews.RemoveNavigationViewRequest{}, Client{})
}
func (Client) RemoveNavigationViewExecute(_ navigationviews.RemoveNavigationViewRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RemoveReaction(_ context.Context, _arg1 int64) messages.RemoveReactionRequest {
	return withAPIService(messages.RemoveReactionRequest{}, Client{})
}
func (Client) RemoveReactionExecute(_ messages.RemoveReactionRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) RenderMessage(ctx context.Context) messages.RenderMessageRequest {
	return withContext(withAPIService(messages.RenderMessageRequest{}, c), ctx)
}
func (Client) RenderMessageExecute(r messages.RenderMessageRequest) (*messages.RenderMessageResponse, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	content := ""
	if value := requestStringPtr(r, "content"); value != nil {
		content = *value
	}
	rendered := content
	if userID, ok := state.renderedMentionUserIDLocked(content); ok {
		rendered = fmt.Sprintf(`<p><span data-user-id="%d">%s</span></p>`, userID, content)
	} else if channelID, ok := state.renderedMentionChannelIDLocked(content); ok {
		rendered = fmt.Sprintf(`<p><a data-stream-id="%d">%s</a></p>`, channelID, content)
	}
	return &messages.RenderMessageResponse{Response: successResponse(), Rendered: rendered}, nil, nil
}
func (Client) ReorderCustomProfileFields(_ context.Context) serverandorganizations.ReorderCustomProfileFieldsRequest {
	return withAPIService(serverandorganizations.ReorderCustomProfileFieldsRequest{}, Client{})
}
func (Client) ReorderCustomProfileFieldsExecute(_ serverandorganizations.ReorderCustomProfileFieldsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReorderLinkifiers(_ context.Context) serverandorganizations.ReorderLinkifiersRequest {
	return withAPIService(serverandorganizations.ReorderLinkifiersRequest{}, Client{})
}
func (Client) ReorderLinkifiersExecute(_ serverandorganizations.ReorderLinkifiersRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ReportMessage(_ context.Context, _arg1 int64) messages.ReportMessageRequest {
	return withAPIService(messages.ReportMessageRequest{}, Client{})
}
func (Client) ReportMessageExecute(_ messages.ReportMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) ResendEmailInvite(_ context.Context, _arg1 int64) invites.ResendEmailInviteRequest {
	return withAPIService(invites.ResendEmailInviteRequest{}, Client{})
}
func (Client) ResendEmailInviteExecute(_ invites.ResendEmailInviteRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RevokeEmailInvite(_ context.Context, _arg1 int64) invites.RevokeEmailInviteRequest {
	return withAPIService(invites.RevokeEmailInviteRequest{}, Client{})
}
func (Client) RevokeEmailInviteExecute(_ invites.RevokeEmailInviteRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) RevokeInviteLink(_ context.Context, _arg1 int64) invites.RevokeInviteLinkRequest {
	return withAPIService(invites.RevokeInviteLinkRequest{}, Client{})
}
func (Client) RevokeInviteLinkExecute(_ invites.RevokeInviteLinkRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) SendInvites(_ context.Context) invites.SendInvitesRequest {
	return withAPIService(invites.SendInvitesRequest{}, Client{})
}
func (Client) SendInvitesExecute(_ invites.SendInvitesRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) SendMessage(ctx context.Context) messages.SendMessageRequest {
	return withContext(withAPIService(messages.SendMessageRequest{}, c), ctx)
}
func (Client) SendMessageExecute(r messages.SendMessageRequest) (*messages.SendMessageResponse, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	recipient := zulip.Recipient{}
	if value := requestRecipientPtr(r, "to"); value != nil {
		recipient = *value
	}
	var recipientType *zulip.RecipientType
	if value := requestRecipientTypePtr(r, "recipientType"); value != nil {
		valueCopy := *value
		recipientType = &valueCopy
	}
	content := ""
	if value := requestStringPtr(r, "content"); value != nil {
		content = *value
	}
	topic := ""
	if value := requestStringPtr(r, "topic"); value != nil {
		topic = *value
	}
	state.lastMessage = &SentMessage{
		Recipient:     recipient,
		RecipientType: recipientType,
		Content:       content,
		Topic:         topic,
	}

	messageID := state.nextMessageID
	if messageID == 0 {
		messageID = 1
	}
	state.nextMessageID = messageID + 1
	return &messages.SendMessageResponse{Response: successResponse(), ID: messageID}, nil, nil
}
func (c Client) SetTypingStatus(ctx context.Context) users.SetTypingStatusRequest {
	return withContext(withAPIService(users.SetTypingStatusRequest{}, c), ctx)
}
func (Client) SetTypingStatusExecute(r users.SetTypingStatusRequest) (*zulip.Response, *http.Response, error) {
	client := requestClient(r)
	state := client.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	recipient := zulip.Recipient{}
	if value := requestRecipientPtr(r, "to"); value != nil {
		recipient = *value
	}
	var recipientType *zulip.RecipientType
	if value := requestRecipientTypePtr(r, "recipientType"); value != nil {
		valueCopy := *value
		recipientType = &valueCopy
	}
	op := zulip.TypingStatusOp("")
	if value := requestTypingStatusOpPtr(r, "op"); value != nil {
		op = *value
	}
	state.typingStatuses = append(state.typingStatuses, TypingStatus{
		Recipient:     recipient,
		RecipientType: recipientType,
		Op:            op,
	})
	return &zulip.Response{Result: zulip.ResponseSuccess}, nil, nil
}
func (Client) SetTypingStatusForMessageEdit(_ context.Context, _arg1 int64) users.SetTypingStatusForMessageEditRequest {
	return withAPIService(users.SetTypingStatusForMessageEditRequest{}, Client{})
}
func (Client) SetTypingStatusForMessageEditExecute(_ users.SetTypingStatusForMessageEditRequest) (*zulip.Response, *http.Response, error) {
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
func (Client) TestNotify(_ context.Context) mobile.TestNotifyRequest {
	return withAPIService(mobile.TestNotifyRequest{}, Client{})
}
func (Client) TestNotifyExecute(_ mobile.TestNotifyRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) TestWelcomeBotCustomMessage(_ context.Context) serverandorganizations.TestWelcomeBotCustomMessageRequest {
	return withAPIService(serverandorganizations.TestWelcomeBotCustomMessageRequest{}, Client{})
}
func (Client) TestWelcomeBotCustomMessageExecute(_ serverandorganizations.TestWelcomeBotCustomMessageRequest) (*serverandorganizations.TestWelcomeBotCustomMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UnmuteUser(_ context.Context, _arg1 int64) users.UnmuteUserRequest {
	return withAPIService(users.UnmuteUserRequest{}, Client{})
}
func (Client) UnmuteUserExecute(_ users.UnmuteUserRequest) (*zulip.Response, *http.Response, error) {
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
func (c Client) UpdateChannel(ctx context.Context, channelID int64) channels.UpdateChannelRequest {
	return withInt64Field(withContext(withAPIService(channels.UpdateChannelRequest{}, c), ctx), "channelID", channelID)
}
func (c Client) UpdateChannelExecute(r channels.UpdateChannelRequest) (*zulip.Response, *http.Response, error) {
	state := c.ensureState()
	channelID := requestInt64(r, "channelID")
	if err := state.waitForTurn(requestContext(r), OperationUpdateChannel, int64Key(channelID)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationUpdateChannel); err != nil {
		return nil, nil, err
	}

	channel, ok := state.channels[channelID]
	if !ok {
		return nil, nil, fmt.Errorf("channel %d not found", channelID)
	}
	if folderIDNone := requestBoolPtr(r, "folderIDNone"); folderIDNone != nil && *folderIDNone {
		channel.channel.FolderID = nil
		state.channels[channelID] = channel
		resp := successResponse()
		return &resp, nil, nil
	}
	if folderID := requestInt64Ptr(r, "folderID"); folderID != nil {
		if _, ok := state.channelFolders[*folderID]; !ok {
			return nil, nil, fmt.Errorf("channel folder %d not found", *folderID)
		}
		id := *folderID
		channel.channel.FolderID = &id
	}
	if isArchived := requestBoolPtr(r, "isArchived"); isArchived != nil {
		channel.channel.IsArchived = *isArchived
	}
	state.channels[channelID] = channel
	resp := successResponse()
	return &resp, nil, nil
}
func (c Client) UpdateChannelFolder(ctx context.Context, channelFolderID int64) channels.UpdateChannelFolderRequest {
	return withInt64Field(
		withContext(withAPIService(channels.UpdateChannelFolderRequest{}, c), ctx),
		"channelFolderID",
		channelFolderID,
	)
}
func (c Client) UpdateChannelFolderExecute(r channels.UpdateChannelFolderRequest) (*zulip.Response, *http.Response, error) {
	state := c.ensureState()
	channelFolderID := requestInt64(r, "channelFolderID")
	if err := state.waitForTurn(requestContext(r), OperationUpdateChannelFolder, int64Key(channelFolderID)); err != nil {
		return nil, nil, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := state.failLocked(OperationUpdateChannelFolder); err != nil {
		return nil, nil, err
	}

	folder, ok := state.channelFolders[channelFolderID]
	if !ok {
		return nil, nil, fmt.Errorf("channel folder %d not found", channelFolderID)
	}
	if name := requestStringPtr(r, "name"); name != nil {
		folder.Name = *name
	}
	if description := requestStringPtr(r, "description"); description != nil {
		folder.Description = *description
	}
	if isArchived := requestBoolPtr(r, "isArchived"); isArchived != nil {
		folder.IsArchived = *isArchived
		if *isArchived {
			for _, channel := range state.channels {
				if channel.channel.FolderID != nil && *channel.channel.FolderID == channelFolderID {
					return nil, nil, fmt.Errorf(
						"You need to remove all the channels from this folder to archive it. (BAD_REQUEST)",
					)
				}
			}
		}
	}
	state.channelFolders[channelFolderID] = folder
	resp := successResponse()
	return &resp, nil, nil
}
func (Client) UpdateLinkifier(_ context.Context, _arg1 int64) serverandorganizations.UpdateLinkifierRequest {
	return withAPIService(serverandorganizations.UpdateLinkifierRequest{}, Client{})
}
func (Client) UpdateLinkifierExecute(_ serverandorganizations.UpdateLinkifierRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessage(_ context.Context, _arg1 int64) messages.UpdateMessageRequest {
	return withAPIService(messages.UpdateMessageRequest{}, Client{})
}
func (Client) UpdateMessageExecute(_ messages.UpdateMessageRequest) (*messages.UpdateMessageResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessageFlags(_ context.Context) messages.UpdateMessageFlagsRequest {
	return withAPIService(messages.UpdateMessageFlagsRequest{}, Client{})
}
func (Client) UpdateMessageFlagsExecute(_ messages.UpdateMessageFlagsRequest) (*messages.UpdateMessageFlagsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateMessageFlagsForNarrow(_ context.Context) messages.UpdateMessageFlagsForNarrowRequest {
	return withAPIService(messages.UpdateMessageFlagsForNarrowRequest{}, Client{})
}
func (Client) UpdateMessageFlagsForNarrowExecute(_ messages.UpdateMessageFlagsForNarrowRequest) (*messages.UpdateMessageFlagsForNarrowResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdatePresence(_ context.Context) users.UpdatePresenceRequest {
	return withAPIService(users.UpdatePresenceRequest{}, Client{})
}
func (Client) UpdatePresenceExecute(_ users.UpdatePresenceRequest) (*users.UpdatePresenceResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateRealmUserSettingsDefaults(_ context.Context) serverandorganizations.UpdateRealmUserSettingsDefaultsRequest {
	return withAPIService(serverandorganizations.UpdateRealmUserSettingsDefaultsRequest{}, Client{})
}
func (Client) UpdateRealmUserSettingsDefaultsExecute(_ serverandorganizations.UpdateRealmUserSettingsDefaultsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateScheduledMessage(_ context.Context, _arg1 int64) scheduledmessages.UpdateScheduledMessageRequest {
	return withAPIService(scheduledmessages.UpdateScheduledMessageRequest{}, Client{})
}
func (Client) UpdateScheduledMessageExecute(_ scheduledmessages.UpdateScheduledMessageRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSettings(_ context.Context) users.UpdateSettingsRequest {
	return withAPIService(users.UpdateSettingsRequest{}, Client{})
}
func (Client) UpdateSettingsExecute(_ users.UpdateSettingsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateStatus(_ context.Context) users.UpdateStatusRequest {
	return withAPIService(users.UpdateStatusRequest{}, Client{})
}
func (Client) UpdateStatusExecute(_ users.UpdateStatusRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateStatusForUser(_ context.Context, _arg1 int64) users.UpdateStatusForUserRequest {
	return withAPIService(users.UpdateStatusForUserRequest{}, Client{})
}
func (Client) UpdateStatusForUserExecute(_ users.UpdateStatusForUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSubscriptionSettings(_ context.Context) channels.UpdateSubscriptionSettingsRequest {
	return withAPIService(channels.UpdateSubscriptionSettingsRequest{}, Client{})
}
func (Client) UpdateSubscriptionSettingsExecute(_ channels.UpdateSubscriptionSettingsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateSubscriptions(_ context.Context) channels.UpdateSubscriptionsRequest {
	return withAPIService(channels.UpdateSubscriptionsRequest{}, Client{})
}
func (Client) UpdateSubscriptionsExecute(_ channels.UpdateSubscriptionsRequest) (*channels.UpdateSubscriptionsResponse, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUser(_ context.Context, _arg1 int64) users.UpdateUserRequest {
	return withAPIService(users.UpdateUserRequest{}, Client{})
}
func (Client) UpdateUserByEmail(_ context.Context, _arg1 string) users.UpdateUserByEmailRequest {
	return withAPIService(users.UpdateUserByEmailRequest{}, Client{})
}
func (Client) UpdateUserByEmailExecute(_ users.UpdateUserByEmailRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUserExecute(_ users.UpdateUserRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (c Client) UpdateUserGroup(ctx context.Context, userGroupID int64) users.UpdateUserGroupRequest {
	return withInt64Field(withContext(withAPIService(users.UpdateUserGroupRequest{}, c), ctx), "userGroupID", userGroupID)
}
func (c Client) UpdateUserGroupExecute(r users.UpdateUserGroupRequest) (*zulip.Response, *http.Response, error) {
	state := c.ensureState()
	state.mu.Lock()
	defer state.mu.Unlock()

	id := requestInt64(r, "userGroupID")
	group, ok := state.userGroups[id]
	if !ok {
		return nil, nil, fmt.Errorf("user group %d not found", id)
	}
	if deactivated := requestBoolPtr(r, "deactivated"); deactivated != nil {
		group.group.Deactivated = *deactivated
	}
	state.userGroups[id] = group
	resp := successResponse()
	return &resp, nil, nil
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
func (Client) UpdateUserGroupSubgroups(_ context.Context, _arg1 int64) users.UpdateUserGroupSubgroupsRequest {
	return withAPIService(users.UpdateUserGroupSubgroupsRequest{}, Client{})
}
func (Client) UpdateUserGroupSubgroupsExecute(_ users.UpdateUserGroupSubgroupsRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UpdateUserTopic(_ context.Context) channels.UpdateUserTopicRequest {
	return withAPIService(channels.UpdateUserTopicRequest{}, Client{})
}
func (Client) UpdateUserTopicExecute(_ channels.UpdateUserTopicRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UploadCustomEmoji(_ context.Context, _arg1 string) serverandorganizations.UploadCustomEmojiRequest {
	return withAPIService(serverandorganizations.UploadCustomEmojiRequest{}, Client{})
}
func (Client) UploadCustomEmojiExecute(_ serverandorganizations.UploadCustomEmojiRequest) (*zulip.Response, *http.Response, error) {
	return nil, nil, nil
}
func (Client) UploadFile(_ context.Context) messages.UploadFileRequest {
	return withAPIService(messages.UploadFileRequest{}, Client{})
}
func (Client) UploadFileExecute(_ messages.UploadFileRequest) (*messages.UploadFileResponse, *http.Response, error) {
	return nil, nil, nil
}
