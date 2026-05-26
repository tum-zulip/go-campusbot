package eventloop

import (
	"github.com/tum-zulip/go-zulip/zulip/events"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// classifyEvent examines a Zulip event and returns the derived channel lifecycle items
// that should be enqueued in the persistent channel lifecycle queue.
//
// Rules:
//   - Heartbeats are never returned.
//   - Message events are never returned.
//   - EventUnmarshalingError entries are never returned.
//   - Channel create/update/delete events produce one item each.
//   - Subscription add/remove/peer events produce one item each.
//   - SubscriptionUpdateEvent (personal settings) is intentionally skipped.
//   - Unknown event types return an empty slice.
func classifyEvent(event events.Event) []storage.ChannelLifecycleEnqueueItem {
	switch e := event.(type) {
	case events.ChannelCreateEvent:
		// One item for the whole event; channel_id/name are null since there may be
		// multiple channels. The full list of channels is in payload_json.
		_ = e
		return []storage.ChannelLifecycleEnqueueItem{{
			LifecycleKind: storage.LifecycleKindChannelCreated,
			Op:            string(events.EventOpCreate),
		}}
	case events.ChannelUpdateEvent:
		// A single ChannelUpdateEvent always refers to one channel.
		var channelIDPtr *int64
		if e.ChannelID != 0 {
			id := e.ChannelID
			channelIDPtr = &id
		}
		return []storage.ChannelLifecycleEnqueueItem{{
			LifecycleKind: storage.LifecycleKindChannelUpdated,
			ChannelID:     channelIDPtr,
			ChannelName:   channelNamePtr(e.Name),
			Op:            string(events.EventOpUpdate),
		}}
	case events.ChannelDeleteEvent:
		// One item for the whole event; channel IDs are in payload_json.
		_ = e
		return []storage.ChannelLifecycleEnqueueItem{{
			LifecycleKind: storage.LifecycleKindChannelDeleted,
			Op:            string(events.EventOpDelete),
		}}
	case events.SubscriptionAddEvent:
		_ = e
		return []storage.ChannelLifecycleEnqueueItem{{
			LifecycleKind: storage.LifecycleKindSubscriptionAdded,
			Op:            string(events.EventOpAdd),
		}}
	case events.SubscriptionRemoveEvent:
		_ = e
		return []storage.ChannelLifecycleEnqueueItem{{
			LifecycleKind: storage.LifecycleKindSubscriptionRemoved,
			Op:            string(events.EventOpRemove),
		}}
	default:
		// Includes: SubscriptionUpdateEvent (personal settings), message events handled
		// by handleMessage, heartbeats handled before handleEvent, EventUnmarshalingError,
		// and all other non-lifecycle event types.
		// All of these are stored as raw events but do not enter the lifecycle queue.
		_ = e
		return nil
	}
}

// channelNamePtr returns a pointer to s if s is non-empty, otherwise nil.
func channelNamePtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
