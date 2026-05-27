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
	"sync"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	realtimeevents "github.com/tum-zulip/go-zulip/zulip/api/real_time_events"
	"github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

const DefaultStreamsTTL = 5 * time.Second

type Streams struct {
	ttl   time.Duration
	state atomic.Pointer[streamsState]

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
	logger *slog.Logger
}

type streamsState struct {
	expiresAt time.Time
	streams   []zulip.Channel
	body      []byte
}

type streamsResponse struct {
	Result  string          `json:"result"`
	Msg     string          `json:"msg"`
	Streams []zulip.Channel `json:"streams"`
}

func NewStreams(ttl time.Duration) *Streams {
	if ttl <= 0 {
		ttl = DefaultStreamsTTL
	}
	return &Streams{ttl: ttl}
}

func (c *Streams) RoundTripper(base http.RoundTripper) http.RoundTripper {
	if c == nil {
		return base
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return streamsTransport{base: base, cache: c}
}

func (c *Streams) Start(ctx context.Context, base client.Client, logger *slog.Logger) error {
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

func (c *Streams) Close() error {
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

func (c *Streams) Invalidate() {
	c.invalidate("manual")
}

func (c *Streams) invalidate(reason string, attrs ...any) {
	if c != nil {
		previous := c.state.Swap(nil)
		if previous != nil {
			c.warnInvalidation(previous, reason, attrs...)
		}
	}
}

func (c *Streams) warnInvalidation(previous *streamsState, reason string, attrs ...any) {
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	baseAttrs := []any{
		"cache", "streams",
		"reason", reason,
		"cached_streams", len(previous.streams),
		"expires_at", previous.expiresAt,
	}
	logger.Warn("invalidated Zulip cache", append(baseAttrs, attrs...)...)
}

func (c *Streams) HandleEvent(event events.Event) {
	if c == nil || event == nil {
		return
	}
	switch event := event.(type) {
	case events.ChannelCreateEvent:
		c.upsert(event.Channels...)
	case events.ChannelDeleteEvent:
		c.remove(channelDeleteIDs(event)...)
	case events.ChannelUpdateEvent:
		c.update(event)
	}
}

func (c *Streams) run(ctx context.Context, base client.Client) {
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		if err := c.consumeQueue(ctx, base); err != nil && ctx.Err() == nil {
			c.invalidate("event_queue_failed", "error", err)
			c.logger.WarnContext(ctx, "streams cache event queue failed", "error", err)
			if !waitRetry(ctx) {
				return
			}
		}
	}
}

func (c *Streams) consumeQueue(ctx context.Context, base client.Client) error {
	resp, _, err := base.RegisterQueue(ctx).
		ApplyMarkdown(false).
		AllPublicChannels(true).
		EventTypes([]events.EventType{events.EventTypeChannel}).
		ClientCapabilities(map[string]interface{}{
			"notification_settings_null": true,
			"archived_channels":          true,
		}).
		Execute()
	if err != nil {
		return fmt.Errorf("register streams cache event queue: %w", err)
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return errors.New("register streams cache event queue: empty queue ID")
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
			c.logger.WarnContext(ctx, "failed to close streams cache event queue", "error", err)
		}
		deleteCtx, cancelDelete := context.WithTimeout(context.Background(), deleteQueueTimeout)
		defer cancelDelete()
		if err := deleteQueue(deleteCtx, base, queueID); err != nil {
			c.logger.WarnContext(ctx, "failed to delete streams cache event queue", "error", err)
		}
	}()
	eventCh, err := queue.Connect(queueCtx, queueID, resp.LastEventID)
	if err != nil {
		return fmt.Errorf("connect streams cache event queue: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return fmt.Errorf("poll streams cache event queue: %w", err)
		case event, ok := <-eventCh:
			if !ok {
				return errors.New("streams cache event queue closed")
			}
			c.HandleEvent(event)
		}
	}
}

func (c *Streams) upsert(streams ...zulip.Channel) {
	c.mutate(func(current []zulip.Channel) []zulip.Channel {
		for _, stream := range streams {
			if stream.ChannelID == 0 {
				continue
			}
			replaced := false
			for i := range current {
				if current[i].ChannelID == stream.ChannelID {
					current[i] = stream
					replaced = true
					break
				}
			}
			if !replaced {
				current = append(current, stream)
			}
		}
		return current
	})
}

func (c *Streams) remove(ids ...int64) {
	if len(ids) == 0 {
		return
	}
	remove := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		remove[id] = struct{}{}
	}
	c.mutate(func(streams []zulip.Channel) []zulip.Channel {
		out := streams[:0]
		for _, stream := range streams {
			if _, ok := remove[stream.ChannelID]; !ok {
				out = append(out, stream)
			}
		}
		return out
	})
}

//nolint:gocognit // Event patch handling mirrors Zulip's nested event shape.
func (c *Streams) update(event events.ChannelUpdateEvent) {
	if event.ChannelID == 0 {
		c.invalidate("channel_update_missing_channel_id", "property", event.Property)
		return
	}
	if event.Property == "is_archived" {
		c.updateArchived(event)
		return
	}
	c.mutate(func(streams []zulip.Channel) []zulip.Channel {
		for i := range streams {
			if streams[i].ChannelID != event.ChannelID {
				continue
			}
			if event.Name != "" {
				streams[i].Name = event.Name
			}
			if event.Property == "folder_id" {
				if event.Value == nil {
					streams[i].FolderID = nil
					return streams
				}
				if event.Value.Int64 != nil {
					folderID := *event.Value.Int64
					streams[i].FolderID = &folderID
					return streams
				}
			}
			if event.Property == "description" && event.Value != nil && event.Value.String != nil {
				streams[i].Description = *event.Value.String
				if event.RenderedDescription != nil {
					streams[i].RenderedDescription = *event.RenderedDescription
				}
				return streams
			}
			c.invalidate(
				"channel_update_unsupported_property",
				"channel_id", event.ChannelID,
				"property", event.Property,
			)
			return streams
		}
		return streams
	})
}

func (c *Streams) updateArchived(event events.ChannelUpdateEvent) {
	if event.Value == nil || event.Value.Bool == nil {
		c.invalidate("channel_update_invalid_is_archived", "channel_id", event.ChannelID)
		return
	}
	if *event.Value.Bool {
		c.remove(event.ChannelID)
		return
	}
	c.mutate(func(streams []zulip.Channel) []zulip.Channel {
		for i := range streams {
			if streams[i].ChannelID == event.ChannelID {
				streams[i].IsArchived = false
				if event.Name != "" {
					streams[i].Name = event.Name
				}
				return streams
			}
		}
		return streams
	})
}

func (c *Streams) mutate(fn func([]zulip.Channel) []zulip.Channel) {
	for {
		current := c.state.Load()
		if current == nil || time.Now().After(current.expiresAt) {
			c.invalidate("mutate_without_fresh_state")
			return
		}
		streams := append([]zulip.Channel(nil), current.streams...)
		nextStreams := fn(streams)
		nextBody, err := marshalStreams(nextStreams)
		if err != nil {
			c.invalidate("marshal_failed", "error", err)
			return
		}
		next := &streamsState{
			expiresAt: current.expiresAt,
			streams:   append([]zulip.Channel(nil), nextStreams...),
			body:      nextBody,
		}
		if c.state.CompareAndSwap(current, next) {
			return
		}
	}
}

type streamsTransport struct {
	base  http.RoundTripper
	cache *Streams
}

func (t streamsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCacheableStreamsRequest(req) {
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

func (c *Streams) cachedBody() []byte {
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

//nolint:dupl // Response caching intentionally mirrors the user-groups cache.
func (c *Streams) storeResponse(resp *http.Response) (*http.Response, error) {
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

	var decoded streamsResponse
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Result != "success" {
		c.invalidate("store_response_decode_failed", "result", decoded.Result, "error", err)
		return resp, nil
	}
	c.state.Store(&streamsState{
		expiresAt: time.Now().Add(c.ttl),
		streams:   append([]zulip.Channel(nil), decoded.Streams...),
		body:      append([]byte(nil), body...),
	})
	return resp, nil
}

func isCacheableStreamsRequest(req *http.Request) bool {
	if req == nil || req.Method != http.MethodGet || req.URL == nil {
		return false
	}
	return req.URL.Path == "/api/v1/streams"
}

func marshalStreams(streams []zulip.Channel) ([]byte, error) {
	return json.Marshal(streamsResponse{
		Result:  "success",
		Msg:     "",
		Streams: streams,
	})
}

func channelDeleteIDs(event events.ChannelDeleteEvent) []int64 {
	if len(event.ChannelIDs) > 0 {
		return append([]int64(nil), event.ChannelIDs...)
	}
	ids := make([]int64, 0, len(event.Channels))
	for _, channel := range event.Channels {
		fields, ok := channel.(map[string]interface{})
		if !ok {
			continue
		}
		for _, key := range []string{"stream_id", "id"} {
			value, ok := fields[key]
			if !ok {
				continue
			}
			switch value := value.(type) {
			case float64:
				ids = append(ids, int64(value))
			case int64:
				ids = append(ids, value)
			case int:
				ids = append(ids, int64(value))
			}
		}
	}
	return ids
}
