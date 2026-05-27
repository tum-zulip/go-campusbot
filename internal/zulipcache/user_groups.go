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

type UserGroups struct {
	ttl   time.Duration
	state atomic.Pointer[userGroupsState]

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *slog.Logger
}

type userGroupsState struct {
	expiresAt time.Time
	groups    []zulip.UserGroup
	body      []byte
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
			"include_deactivated_groups": false,
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
	if group.ID == 0 || group.Deactivated {
		c.remove(group.ID)
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
				if *event.Data.Deactivated {
					return append(groups[:i], groups[i+1:]...)
				}
				groups[i].Deactivated = false
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
		nextGroups := fn(groups)
		nextBody, err := marshalUserGroups(nextGroups)
		if err != nil {
			c.invalidate("marshal_failed", "error", err)
			return
		}
		next := &userGroupsState{
			expiresAt: current.expiresAt,
			groups:    append([]zulip.UserGroup(nil), nextGroups...),
			body:      nextBody,
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
	if !isCacheableUserGroupsRequest(req) {
		return t.base.RoundTrip(req)
	}
	if body := t.cache.cachedBody(); body != nil {
		return cachedResponse(req, body), nil
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	return t.cache.storeResponse(resp)
}

func (c *UserGroups) cachedBody() []byte {
	current := c.state.Load()
	if current == nil {
		return nil
	}
	if time.Now().After(current.expiresAt) {
		c.invalidate("cached_body_expired")
		return nil
	}
	return append([]byte(nil), current.body...)
}

//nolint:dupl // Response caching intentionally mirrors the streams cache.
func (c *UserGroups) storeResponse(resp *http.Response) (*http.Response, error) {
	if resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		c.invalidate("store_response_not_cacheable", "status_code", statusCode)
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	if closeErr := resp.Body.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		c.invalidate("store_response_read_failed", "error", err)
		return resp, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))

	var decoded userGroupsResponse
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Result != "success" {
		c.invalidate("store_response_decode_failed", "result", decoded.Result, "error", err)
		return resp, nil
	}
	c.state.Store(&userGroupsState{
		expiresAt: time.Now().Add(c.ttl),
		groups:    append([]zulip.UserGroup(nil), decoded.UserGroups...),
		body:      append([]byte(nil), body...),
	})
	return resp, nil
}

func isCacheableUserGroupsRequest(req *http.Request) bool {
	if req == nil || req.Method != http.MethodGet || req.URL == nil {
		return false
	}
	if req.URL.Path != "/api/v1/user_groups" {
		return false
	}
	query := req.URL.Query()
	values, ok := query["include_deactivated_groups"]
	if !ok {
		return true
	}
	return len(values) == 1 && values[0] == "false"
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
