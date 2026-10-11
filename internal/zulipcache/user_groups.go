package zulipcache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	"github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

const (
	DefaultUserGroupsTTL = 5 * time.Second
	queueRetryDelay      = time.Second
	deleteQueueTimeout   = 5 * time.Second
)

// errResponseNotCacheable reports a user-groups response that was passed
// through without being cached, such as an API error.
var errResponseNotCacheable = errors.New("user-groups response not cacheable")

type UserGroups struct {
	ttl   time.Duration
	state atomic.Pointer[userGroupsState]

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *slog.Logger
}

// userGroupsState holds every user group of the realm, including deactivated
// ones, so both variants of the listing can be served from one snapshot.
type userGroupsState struct {
	expiresAt  time.Time
	groups     []zulip.UserGroup
	activeBody []byte
	allBody    []byte
}

func newUserGroupsState(expiresAt time.Time, groups []zulip.UserGroup) (*userGroupsState, error) {
	active := make([]zulip.UserGroup, 0, len(groups))
	for _, group := range groups {
		if !group.Deactivated {
			active = append(active, group)
		}
	}
	activeBody, err := marshalUserGroups(active)
	if err != nil {
		return nil, err
	}
	allBody, err := marshalUserGroups(groups)
	if err != nil {
		return nil, err
	}
	return &userGroupsState{
		expiresAt:  expiresAt,
		groups:     append([]zulip.UserGroup(nil), groups...),
		activeBody: activeBody,
		allBody:    allBody,
	}, nil
}

func (s *userGroupsState) body(includeDeactivated bool) []byte {
	if includeDeactivated {
		return s.allBody
	}
	return s.activeBody
}

type userGroupsResponse struct {
	Result     string            `json:"result"`
	Msg        string            `json:"msg"`
	UserGroups []zulip.UserGroup `json:"user_groups"`
}

func NewUserGroups(ttl time.Duration) *UserGroups {
	if ttl <= 0 {
		ttl = DefaultUserGroupsTTL
	}
	return &UserGroups{ttl: ttl}
}

func (c *UserGroups) RoundTripper(base http.RoundTripper) http.RoundTripper {
	if c == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return userGroupsTransport{base: base, cache: c}
}

func (c *UserGroups) Start(ctx context.Context, base client.Client, logger *slog.Logger) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("context must not be nil")
	}
	if base == nil {
		return errors.New("zulip client must not be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.logger = logger
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run(runCtx, base)
	}()
	return nil
}

func (c *UserGroups) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	return nil
}

func (c *UserGroups) Invalidate() {
	c.invalidate("manual")
}

func (c *UserGroups) invalidate(reason string, attrs ...any) {
	if c != nil {
		previous := c.state.Swap(nil)
		if previous != nil {
			c.warnInvalidation(previous, reason, attrs...)
		}
	}
}

func (c *UserGroups) warnInvalidation(previous *userGroupsState, reason string, attrs ...any) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	baseAttrs := []any{
		"cache", "user_groups",
		"reason", reason,
		"cached_groups", len(previous.groups),
		"expires_at", previous.expiresAt,
	}
	logger.Warn("invalidated Zulip cache", append(baseAttrs, attrs...)...)
}

func (c *UserGroups) HandleEvent(event events.Event) {
	if c == nil || event == nil {
		return
	}
	switch event := event.(type) {
	case events.UserGroupAddEvent:
		c.upsert(event.Group)
	case events.UserGroupRemoveEvent:
		c.remove(event.GroupID)
	case events.UserGroupUpdateEvent:
		c.update(event)
	case events.UserGroupMembersEvent:
		c.updateMembers(event)
	case events.UserGroupSubgroupsEvent:
		c.updateSubgroups(event)
	}
}

func (c *UserGroups) run(ctx context.Context, base client.Client) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := c.consumeQueue(ctx, base); err != nil && ctx.Err() == nil {
			c.invalidate("event_queue_failed", "error", err)
			c.logger.WarnContext(ctx, "user-groups cache event queue failed", "error", err)
			if !waitRetry(ctx) {
				return
			}
		}
	}
}

func (c *UserGroups) consumeQueue(ctx context.Context, base client.Client) error {
	resp, _, err := base.RegisterQueue(ctx).
		ApplyMarkdown(false).
		EventTypes([]events.EventType{events.EventTypeUserGroup}).
		ClientCapabilities(map[string]interface{}{
			"notification_settings_null": true,
			"include_deactivated_groups": true,
		}).
		Execute()
	if err != nil {
		return fmt.Errorf("register user-groups cache event queue: %w", err)
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return errors.New("register user-groups cache event queue: empty queue ID")
	}
	queueID := *resp.QueueID
	errs := make(chan error, 1)
	queue := realtimeevents.NewEventQueue(
		base,
		realtimeevents.WithLogger(c.logger),
		realtimeevents.WithEventQueueChannelErrorHandler(c.logger, errs),
	)
	queueCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		if err := queue.Close(); err != nil {
			c.logger.WarnContext(ctx, "failed to close user-groups cache event queue", "error", err)
		}
		deleteCtx, cancelDelete := context.WithTimeout(context.Background(), deleteQueueTimeout)
		defer cancelDelete()
		if err := deleteQueue(deleteCtx, base, queueID); err != nil {
			c.logger.WarnContext(ctx, "failed to delete user-groups cache event queue", "error", err)
		}
	}()
	eventCh, err := queue.Connect(queueCtx, queueID, resp.LastEventID)
	if err != nil {
		return fmt.Errorf("connect user-groups cache event queue: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return fmt.Errorf("poll user-groups cache event queue: %w", err)
		case event, ok := <-eventCh:
			if !ok {
				return errors.New("user-groups cache event queue closed")
			}
			c.HandleEvent(event)
		}
	}
}

func deleteQueue(ctx context.Context, base client.Client, queueID string) error {
	if queueID == "" {
		return nil
	}
	_, _, err := base.DeleteQueue(ctx).QueueID(queueID).Execute()
	return err
}

func waitRetry(ctx context.Context) bool {
	timer := time.NewTimer(queueRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *UserGroups) upsert(group zulip.UserGroup) {
	if group.ID == 0 {
		return
	}
	c.mutate(func(groups []zulip.UserGroup) []zulip.UserGroup {
		for i := range groups {
			if groups[i].ID == group.ID {
				groups[i] = group
				return groups
			}
		}
		return append(groups, group)
	})
}

func (c *UserGroups) remove(groupID int64) {
	if groupID == 0 {
		return
	}
	c.mutate(func(groups []zulip.UserGroup) []zulip.UserGroup {
		for i := range groups {
			if groups[i].ID == groupID {
				return append(groups[:i], groups[i+1:]...)
			}
		}
		return groups
	})
}

//nolint:gocognit // Event patch handling mirrors Zulip's nested event shape.
func (c *UserGroups) update(event events.UserGroupUpdateEvent) {
	c.mutate(func(groups []zulip.UserGroup) []zulip.UserGroup {
		for i := range groups {
			if groups[i].ID != event.GroupID {
				continue
			}
			if event.Data.Deactivated != nil {
				groups[i].Deactivated = *event.Data.Deactivated
			}
			if event.Data.Name != nil {
				groups[i].Name = *event.Data.Name
			}
			if event.Data.Description != nil {
				groups[i].Description = *event.Data.Description
			}
			if event.Data.CanAddMembersGroup != nil {
				groups[i].CanAddMembersGroup = *event.Data.CanAddMembersGroup
			}
			if event.Data.CanJoinGroup != nil {
				groups[i].CanJoinGroup = *event.Data.CanJoinGroup
			}
			if event.Data.CanLeaveGroup != nil {
				groups[i].CanLeaveGroup = *event.Data.CanLeaveGroup
			}
			if event.Data.CanManageGroup != nil {
				groups[i].CanManageGroup = *event.Data.CanManageGroup
			}
			if event.Data.CanMentionGroup != nil {
				groups[i].CanMentionGroup = *event.Data.CanMentionGroup
			}
			if event.Data.CanRemoveMembersGroup != nil {
				groups[i].CanRemoveMembersGroup = *event.Data.CanRemoveMembersGroup
			}
			return groups
		}
		return groups
	})
}

func (c *UserGroups) updateMembers(event events.UserGroupMembersEvent) {
	op, ok := event.GetOp()
	if !ok {
		c.invalidate("user_group_members_event_missing_op", "group_id", event.GroupID)
		return
	}
	c.mutate(func(groups []zulip.UserGroup) []zulip.UserGroup {
		for i := range groups {
			if groups[i].ID != event.GroupID {
				continue
			}
			//nolint:exhaustive // Other event ops invalidate the cache through the default branch.
			switch op {
			case events.EventOpAddMembers:
				groups[i].Members = appendMissingInt64s(groups[i].Members, event.UserIDs)
			case events.EventOpRemoveMembers:
				groups[i].Members = removeInt64s(groups[i].Members, event.UserIDs)
			default:
				c.invalidate(
					"user_group_members_event_unknown_op",
					"group_id", event.GroupID,
					"op", op,
				)
			}
			return groups
		}
		return groups
	})
}

func (c *UserGroups) updateSubgroups(event events.UserGroupSubgroupsEvent) {
	op, ok := event.GetOp()
	if !ok {
		c.invalidate("user_group_subgroups_event_missing_op", "group_id", event.GroupID)
		return
	}
	c.mutate(func(groups []zulip.UserGroup) []zulip.UserGroup {
		for i := range groups {
			if groups[i].ID != event.GroupID {
				continue
			}
			//nolint:exhaustive // Other event ops invalidate the cache through the default branch.
			switch op {
			case events.EventOpAddSubgroups:
				groups[i].DirectSubgroupIDs = appendMissingInt64s(
					groups[i].DirectSubgroupIDs,
					event.DirectSubgroupIDs,
				)
			case events.EventOpRemoveSubgroups:
				groups[i].DirectSubgroupIDs = removeInt64s(
					groups[i].DirectSubgroupIDs,
					event.DirectSubgroupIDs,
				)
			default:
				c.invalidate(
					"user_group_subgroups_event_unknown_op",
					"group_id", event.GroupID,
					"op", op,
				)
			}
			return groups
		}
		return groups
	})
}

func (c *UserGroups) mutate(fn func([]zulip.UserGroup) []zulip.UserGroup) {
	for {
		current := c.state.Load()
		if current == nil || time.Now().After(current.expiresAt) {
			c.invalidate("mutate_without_fresh_state")
			return
		}
		groups := append([]zulip.UserGroup(nil), current.groups...)
		next, err := newUserGroupsState(current.expiresAt, fn(groups))
		if err != nil {
			c.invalidate("marshal_failed", "error", err)
			return
		}
		if c.state.CompareAndSwap(current, next) {
			return
		}
	}
}

type userGroupsTransport struct {
	base  http.RoundTripper
	cache *UserGroups
}

func (t userGroupsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	includeDeactivated, ok := cacheableUserGroupsRequest(req)
	if !ok || bypassed(req) {
		return t.base.RoundTrip(req)
	}
	if body := t.cache.cachedBody(includeDeactivated); body != nil {
		return cachedResponse(req, body), nil
	}
	// Always fetch every group, deactivated ones included, so the snapshot
	// can answer both variants of the request.
	resp, err := t.base.RoundTrip(withAllUserGroups(req))
	if err != nil {
		return resp, err
	}
	stored, err := t.cache.storeResponse(resp)
	if errors.Is(err, errResponseNotCacheable) {
		return resp, nil
	}
	if err != nil {
		return resp, err
	}
	return cachedResponse(req, stored.body(includeDeactivated)), nil
}

func withAllUserGroups(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	query := clone.URL.Query()
	query.Set("include_deactivated_groups", "true")
	clone.URL.RawQuery = query.Encode()
	return clone
}

func (c *UserGroups) cachedBody(includeDeactivated bool) []byte {
	current := c.state.Load()
	if current == nil {
		return nil
	}
	if time.Now().After(current.expiresAt) {
		c.invalidate("cached_body_expired")
		return nil
	}
	return append([]byte(nil), current.body(includeDeactivated)...)
}

// storeResponse caches a successful listing of every user group. It returns
// errResponseNotCacheable when resp is not cacheable; resp's body stays
// readable either way.
func (c *UserGroups) storeResponse(resp *http.Response) (*userGroupsState, error) {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		c.invalidate("store_response_not_cacheable", "status_code", statusCode)
		return nil, errResponseNotCacheable
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		c.invalidate("store_response_read_failed", "error", err)
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	var decoded userGroupsResponse
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Result != "success" {
		c.invalidate("store_response_decode_failed", "result", decoded.Result, "error", err)
		return nil, errResponseNotCacheable
	}
	state, err := newUserGroupsState(time.Now().Add(c.ttl), decoded.UserGroups)
	if err != nil {
		c.invalidate("marshal_failed", "error", err)
		return nil, errResponseNotCacheable
	}
	c.state.Store(state)
	return state, nil
}

// cacheableUserGroupsRequest reports whether req asks for deactivated user
// groups too, and whether it is a user-groups listing the cache can serve.
func cacheableUserGroupsRequest(req *http.Request) (bool, bool) {
	if req == nil || req.Method != http.MethodGet || req.URL == nil {
		return false, false
	}
	if req.URL.Path != "/api/v1/user_groups" {
		return false, false
	}
	values, ok := req.URL.Query()["include_deactivated_groups"]
	if !ok {
		return false, true
	}
	if len(values) != 1 {
		return false, false
	}
	switch values[0] {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

func cachedResponse(req *http.Request, body []byte) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.Itoa(len(body)))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func marshalUserGroups(groups []zulip.UserGroup) ([]byte, error) {
	return json.Marshal(userGroupsResponse{
		Result:     "success",
		Msg:        "",
		UserGroups: groups,
	})
}

func appendMissingInt64s(values []int64, added []int64) []int64 {
	for _, value := range added {
		found := false
		for _, existing := range values {
			if existing == value {
				found = true
				break
			}
		}
		if !found {
			values = append(values, value)
		}
	}
	return values
}

func removeInt64s(values []int64, removed []int64) []int64 {
	remove := make(map[int64]struct{}, len(removed))
	for _, value := range removed {
		remove[value] = struct{}{}
	}
	out := values[:0]
	for _, value := range values {
		if _, ok := remove[value]; !ok {
			out = append(out, value)
		}
	}
	return out
}
