package eventloop

import (
	"encoding/json"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestClassifyEvent_MessageEventNotEnqueued(t *testing.T) {
	t.Parallel()

	event := mustMessageEvent(t, 1, 100, "help")
	items := classifyEvent(event)
	if len(items) != 0 {
		t.Fatalf("classifyEvent(MessageEvent) = %v, want empty", items)
	}
}

func TestClassifyEvent_HeartbeatNotEnqueued(t *testing.T) {
	t.Parallel()

	event := mustHeartbeatEvent(t, 1)
	items := classifyEvent(event)
	if len(items) != 0 {
		t.Fatalf("classifyEvent(HeartbeatEvent) = %v, want empty", items)
	}
}

func TestClassifyEvent_ChannelCreateEnqueued(t *testing.T) {
	t.Parallel()

	event := mustChannelCreateEvent(t, 5)
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(ChannelCreateEvent) len = %d, want 1", len(items))
	}
	if items[0].LifecycleKind != storage.LifecycleKindChannelCreated {
		t.Errorf("LifecycleKind = %q, want %q", items[0].LifecycleKind, storage.LifecycleKindChannelCreated)
	}
	if items[0].Op != string(events.EventOpCreate) {
		t.Errorf("Op = %q, want %q", items[0].Op, events.EventOpCreate)
	}
	if items[0].ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil (multiple channels possible)", items[0].ChannelID)
	}
}

func TestClassifyEvent_ChannelUpdateEnqueued(t *testing.T) {
	t.Parallel()

	channelID := int64(42)
	event := mustChannelUpdateEvent(t, 5, channelID, "my-channel")
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(ChannelUpdateEvent) len = %d, want 1", len(items))
	}
	if items[0].LifecycleKind != storage.LifecycleKindChannelUpdated {
		t.Errorf("LifecycleKind = %q, want %q", items[0].LifecycleKind, storage.LifecycleKindChannelUpdated)
	}
	if items[0].Op != string(events.EventOpUpdate) {
		t.Errorf("Op = %q, want %q", items[0].Op, events.EventOpUpdate)
	}
	if items[0].ChannelID == nil || *items[0].ChannelID != channelID {
		t.Errorf("ChannelID = %v, want %d", items[0].ChannelID, channelID)
	}
	if items[0].ChannelName == nil || *items[0].ChannelName != "my-channel" {
		t.Errorf("ChannelName = %v, want \"my-channel\"", items[0].ChannelName)
	}
}

func TestClassifyEvent_ChannelUpdateWithZeroIDSetsNilChannelID(t *testing.T) {
	t.Parallel()

	// channel_id=0 means missing/not set; classifier should produce nil.
	event := mustChannelUpdateEvent(t, 5, 0, "")
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(ChannelUpdateEvent zero id) len = %d, want 1", len(items))
	}
	if items[0].ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil for zero channel_id", items[0].ChannelID)
	}
	if items[0].ChannelName != nil {
		t.Errorf("ChannelName = %v, want nil for empty name", items[0].ChannelName)
	}
}

func TestClassifyEvent_ChannelDeleteEnqueued(t *testing.T) {
	t.Parallel()

	event := mustChannelDeleteEvent(t, 5, []int64{1, 2, 3})
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(ChannelDeleteEvent) len = %d, want 1", len(items))
	}
	if items[0].LifecycleKind != storage.LifecycleKindChannelDeleted {
		t.Errorf("LifecycleKind = %q, want %q", items[0].LifecycleKind, storage.LifecycleKindChannelDeleted)
	}
	if items[0].Op != string(events.EventOpDelete) {
		t.Errorf("Op = %q, want %q", items[0].Op, events.EventOpDelete)
	}
	if items[0].ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil (multiple channels possible)", items[0].ChannelID)
	}
}

func TestClassifyEvent_SubscriptionAddEnqueued(t *testing.T) {
	t.Parallel()

	event := mustSubscriptionEvent(t, 5, string(events.EventOpAdd))
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(SubscriptionAddEvent) len = %d, want 1", len(items))
	}
	if items[0].LifecycleKind != storage.LifecycleKindSubscriptionAdded {
		t.Errorf("LifecycleKind = %q, want %q", items[0].LifecycleKind, storage.LifecycleKindSubscriptionAdded)
	}
	if items[0].Op != string(events.EventOpAdd) {
		t.Errorf("Op = %q, want %q", items[0].Op, events.EventOpAdd)
	}
}

func TestClassifyEvent_SubscriptionRemoveEnqueued(t *testing.T) {
	t.Parallel()

	event := mustSubscriptionEvent(t, 5, string(events.EventOpRemove))
	items := classifyEvent(event)
	if len(items) != 1 {
		t.Fatalf("classifyEvent(SubscriptionRemoveEvent) len = %d, want 1", len(items))
	}
	if items[0].LifecycleKind != storage.LifecycleKindSubscriptionRemoved {
		t.Errorf("LifecycleKind = %q, want %q", items[0].LifecycleKind, storage.LifecycleKindSubscriptionRemoved)
	}
	if items[0].Op != string(events.EventOpRemove) {
		t.Errorf("Op = %q, want %q", items[0].Op, events.EventOpRemove)
	}
}

func TestClassifyEvent_SubscriptionUpdateNotEnqueued(t *testing.T) {
	t.Parallel()

	// SubscriptionUpdateEvent is for personal settings only; skip for lifecycle queue.
	event := mustSubscriptionEvent(t, 5, string(events.EventOpUpdate))
	items := classifyEvent(event)
	if len(items) != 0 {
		t.Fatalf("classifyEvent(SubscriptionUpdateEvent) = %v, want empty (personal settings only)", items)
	}
}

func TestClassifyEvent_UnmarshalingErrorNotEnqueued(t *testing.T) {
	t.Parallel()

	// An EventUnmarshalingError should produce no lifecycle items.
	errEvent := &events.EventUnmarshalingError{}
	items := classifyEvent(errEvent)
	if len(items) != 0 {
		t.Fatalf("classifyEvent(EventUnmarshalingError) = %v, want empty", items)
	}
}

func TestClassifyEvent_UnknownEventNotEnqueued(t *testing.T) {
	t.Parallel()

	// A non-lifecycle event (e.g. presence) should produce no lifecycle items.
	event := mustPresenceEvent(t, 5)
	items := classifyEvent(event)
	if len(items) != 0 {
		t.Fatalf("classifyEvent(unknown) = %v, want empty", items)
	}
}

// mustChannelCreateEvent creates a ChannelCreateEvent via JSON unmarshaling.
func mustChannelCreateEvent(t *testing.T, eventID int64) events.ChannelCreateEvent {
	t.Helper()

	data, err := json.Marshal(map[string]interface{}{
		"id":      eventID,
		"type":    "stream",
		"op":      "create",
		"streams": []map[string]interface{}{{"stream_id": 10, "name": "general"}},
	})
	if err != nil {
		t.Fatalf("marshal ChannelCreateEvent: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal ChannelCreateEvent: %v", err)
	}
	e, ok := envelope.Event.(events.ChannelCreateEvent)
	if !ok {
		t.Fatalf("event type = %T, want events.ChannelCreateEvent", envelope.Event)
	}
	return e
}

// mustChannelUpdateEvent creates a ChannelUpdateEvent via JSON unmarshaling.
func mustChannelUpdateEvent(t *testing.T, eventID, channelID int64, name string) events.ChannelUpdateEvent {
	t.Helper()

	data, err := json.Marshal(map[string]interface{}{
		"id":        eventID,
		"type":      "stream",
		"op":        "update",
		"stream_id": channelID,
		"name":      name,
		"property":  "name",
	})
	if err != nil {
		t.Fatalf("marshal ChannelUpdateEvent: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal ChannelUpdateEvent: %v", err)
	}
	e, ok := envelope.Event.(events.ChannelUpdateEvent)
	if !ok {
		t.Fatalf("event type = %T, want events.ChannelUpdateEvent", envelope.Event)
	}
	return e
}

// mustChannelDeleteEvent creates a ChannelDeleteEvent via JSON unmarshaling.
func mustChannelDeleteEvent(t *testing.T, eventID int64, channelIDs []int64) events.ChannelDeleteEvent {
	t.Helper()

	data, err := json.Marshal(map[string]interface{}{
		"id":         eventID,
		"type":       "stream",
		"op":         "delete",
		"stream_ids": channelIDs,
		"streams":    []interface{}{},
	})
	if err != nil {
		t.Fatalf("marshal ChannelDeleteEvent: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal ChannelDeleteEvent: %v", err)
	}
	e, ok := envelope.Event.(events.ChannelDeleteEvent)
	if !ok {
		t.Fatalf("event type = %T, want events.ChannelDeleteEvent", envelope.Event)
	}
	return e
}

// mustSubscriptionEvent creates a subscription event with the given op via JSON unmarshaling.
// op can be "add", "remove", or "update".
func mustSubscriptionEvent(t *testing.T, eventID int64, op string) events.Event {
	t.Helper()

	var payload map[string]interface{}
	switch op {
	case "add", "peer_add":
		payload = map[string]interface{}{
			"id":            eventID,
			"type":          "subscription",
			"op":            op,
			"subscriptions": []map[string]interface{}{{"stream_id": 1, "name": "general"}},
		}
	case "remove", "peer_remove":
		payload = map[string]interface{}{
			"id":            eventID,
			"type":          "subscription",
			"op":            op,
			"subscriptions": []map[string]interface{}{{"stream_id": 1, "name": "general"}},
		}
	case "update":
		payload = map[string]interface{}{
			"id":        eventID,
			"type":      "subscription",
			"op":        op,
			"stream_id": 1,
			"property":  "is_muted",
			"value":     false,
		}
	default:
		t.Fatalf("unsupported op: %q", op)
	}

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal subscription event: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal subscription event: %v", err)
	}
	return envelope.Event
}

// mustPresenceEvent creates a non-lifecycle event (presence) via JSON unmarshaling.
func mustPresenceEvent(t *testing.T, eventID int64) events.Event {
	t.Helper()

	data, err := json.Marshal(map[string]interface{}{
		"id":   eventID,
		"type": "presence",
		"email": "user@example.com",
		"server_timestamp": 1234567890.0,
		"presence": map[string]interface{}{
			"website": map[string]interface{}{
				"status":    "active",
				"timestamp": 1234567890,
				"client":    "website",
				"pushable":  false,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal presence event: %v", err)
	}
	var envelope events.EventEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("unmarshal presence event: %v", err)
	}
	return envelope.Event
}
