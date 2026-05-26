package zulipbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	z "github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"
	"github.com/tum-zulip/go-zulip/zulip/events"
)

var ErrBadEventQueueID = errors.New("bad Zulip event queue ID")

type QueueState struct {
	QueueID     string
	LastEventID int64
}

// RegisterOptions holds options for registering a Zulip event queue.
// It is intentionally empty for now; kept for forward compatibility.
type RegisterOptions struct{}

type registerQueueSettings struct {
	AllPublicChannels bool
	EventTypes        []events.EventType
	FetchEventTypes   []events.EventType
	Narrow            *z.Narrow
}

type registerQueueResponse struct {
	QueueID                          *string `json:"queue_id,omitempty"`
	LastEventID                      int64   `json:"last_event_id,omitempty"`
	MaxMessageID                     *int64  `json:"max_message_id,omitempty"`
	EventQueueLongpollTimeoutSeconds *int64  `json:"event_queue_longpoll_timeout_seconds,omitempty"`
	IdleQueueTimeoutSecs             *int64  `json:"idle_queue_timeout_secs,omitempty"`
}

func broadQueueRegisterSettings() registerQueueSettings {
	return registerQueueSettings{
		AllPublicChannels: true,
	}
}

type Source interface {
	Register(ctx context.Context, opts RegisterOptions) (QueueState, error)
	Check(ctx context.Context, state QueueState) error
	Poll(ctx context.Context, state QueueState) ([]events.Event, error)
	Delete(ctx context.Context, queueID string) error
}

type ZulipSource struct {
	client zulipclient.Client
}

func NewZulipSource(client zulipclient.Client) *ZulipSource {
	return &ZulipSource{client: client}
}

// Register registers a broad Zulip event queue that subscribes to all public
// channels. We always use AllPublicChannels(true) so the bot receives events
// from every public channel without needing to be subscribed to them individually.
func (source *ZulipSource) Register(ctx context.Context, opts RegisterOptions) (QueueState, error) {
	settings := broadQueueRegisterSettings()
	resp, httpResp, err := source.client.RegisterQueue(ctx).
		ApplyMarkdown(false).
		AllPublicChannels(settings.AllPublicChannels).
		ClientCapabilities(map[string]interface{}{
			"empty_topic_name":           true,
			"notification_settings_null": true,
			"user_settings_object":       true,
		}).
		Execute()
	if err != nil {
		if state, decodeErr := queueStateFromRegisterHTTPResponse(httpResp); decodeErr == nil {
			return state, nil
		}
		return QueueState{}, fmt.Errorf("register Zulip event queue: %w", err)
	}
	if state, decodeErr := queueStateFromRegisterHTTPResponse(httpResp); decodeErr == nil {
		return state, nil
	}
	if resp == nil || resp.QueueID == nil || *resp.QueueID == "" {
		return QueueState{}, errors.New("register Zulip event queue: empty queue ID")
	}
	return QueueState{QueueID: *resp.QueueID, LastEventID: resp.LastEventID}, nil
}

func queueStateFromRegisterHTTPResponse(httpResp *http.Response) (QueueState, error) {
	if httpResp == nil || httpResp.Body == nil {
		return QueueState{}, errors.New("missing Zulip register response body")
	}
	if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
		return QueueState{}, fmt.Errorf("Zulip register returned HTTP %s", httpResp.Status)
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return QueueState{}, err
	}
	httpResp.Body = io.NopCloser(bytes.NewReader(body))

	var resp registerQueueResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return QueueState{}, err
	}
	if resp.QueueID == nil || *resp.QueueID == "" {
		return QueueState{}, errors.New("empty queue ID")
	}
	return QueueState{QueueID: *resp.QueueID, LastEventID: resp.LastEventID}, nil
}

func (source *ZulipSource) Check(ctx context.Context, state QueueState) error {
	_, _, err := source.client.GetEvents(ctx).
		QueueID(state.QueueID).
		LastEventID(state.LastEventID).
		DontBlock(true).
		Execute()
	if err != nil {
		if IsBadEventQueueID(err) {
			return ErrBadEventQueueID
		}
		return fmt.Errorf("check Zulip event queue: %w", err)
	}
	return nil
}

func (source *ZulipSource) Poll(ctx context.Context, state QueueState) ([]events.Event, error) {
	resp, _, err := source.client.GetEvents(ctx).
		QueueID(state.QueueID).
		LastEventID(state.LastEventID).
		Execute()
	if err != nil {
		if IsBadEventQueueID(err) {
			return nil, ErrBadEventQueueID
		}
		return nil, fmt.Errorf("poll Zulip event queue: %w", err)
	}
	if resp == nil {
		return nil, errors.New("poll Zulip event queue: empty response")
	}
	return resp.Events, nil
}

func (source *ZulipSource) Delete(ctx context.Context, queueID string) error {
	if queueID == "" {
		return nil
	}
	_, _, err := source.client.DeleteQueue(ctx).QueueID(queueID).Execute()
	if err != nil {
		return fmt.Errorf("delete Zulip event queue: %w", err)
	}
	return nil
}

func IsBadEventQueueID(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBadEventQueueID) {
		return true
	}
	var badQueue z.BadEventQueueIDError
	if errors.As(err, &badQueue) {
		return true
	}
	var coded z.CodedError
	return errors.As(err, &coded) && coded.Code == "BAD_EVENT_QUEUE_ID"
}
