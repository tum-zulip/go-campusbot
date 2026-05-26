package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

func TestRepositoryPersistsCoreState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	version, err := repo.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() failed: %v", err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}

	if err := repo.SetConfigValue(ctx, ConfigChange{Key: "command_prefix", Value: "!bot", ActorUserID: 10}); err != nil {
		t.Fatalf("SetConfigValue() failed: %v", err)
	}
	value, ok, err := repo.ConfigValue(ctx, "command_prefix")
	if err != nil {
		t.Fatalf("ConfigValue() failed: %v", err)
	}
	if !ok || value != "!bot" {
		t.Fatalf("config value = %q, ok=%v", value, ok)
	}

	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}
	role, ok, err := repo.UserRole(ctx, 10)
	if err != nil {
		t.Fatalf("UserRole() failed: %v", err)
	}
	if !ok || role != permissions.RoleAdmin {
		t.Fatalf("role = %q, ok=%v", role, ok)
	}

	if err := repo.SaveEventQueueState(ctx, EventQueueState{QueueID: "queue-1", LastEventID: 42}); err != nil {
		t.Fatalf("SaveEventQueueState() failed: %v", err)
	}
	state, ok, err := repo.EventQueueState(ctx)
	if err != nil {
		t.Fatalf("EventQueueState() failed: %v", err)
	}
	if !ok || state.QueueID != "queue-1" || state.LastEventID != 42 {
		t.Fatalf("queue state = %#v, ok=%v", state, ok)
	}

	if err := repo.MarkMessageProcessed(ctx, 77); err != nil {
		t.Fatalf("MarkMessageProcessed() failed: %v", err)
	}
	processed, err := repo.MessageProcessed(ctx, 77)
	if err != nil {
		t.Fatalf("MessageProcessed() failed: %v", err)
	}
	if !processed {
		t.Fatal("message should be marked processed")
	}
}

func TestRepositoryTracksRestartRequests(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	target := model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10, 11}}
	id, err := repo.CreateRestartRequest(ctx, RestartRequest{
		RequestedByUserID: 10,
		RequestMessageID:  200,
		Target:            target,
	})
	if err != nil {
		t.Fatalf("CreateRestartRequest() failed: %v", err)
	}
	if id == 0 {
		t.Fatal("restart request ID should be non-zero")
	}

	pending, ok, err := repo.PendingRestartRequest(ctx)
	if err != nil {
		t.Fatalf("PendingRestartRequest() failed: %v", err)
	}
	if !ok || pending.ID != id || pending.Target.UserIDs[1] != 11 {
		t.Fatalf("pending restart = %#v, ok=%v", pending, ok)
	}

	if err := repo.MarkRestartInProgress(ctx, id); err != nil {
		t.Fatalf("MarkRestartInProgress() failed: %v", err)
	}
	pending, ok, err = repo.PendingRestartRequest(ctx)
	if err != nil {
		t.Fatalf("PendingRestartRequest() after in progress failed: %v", err)
	}
	if !ok || pending.ID != id {
		t.Fatalf("in-progress restart = %#v, ok=%v", pending, ok)
	}

	if err := repo.CompleteRestartRequest(ctx, id, 300, ""); err != nil {
		t.Fatalf("CompleteRestartRequest() failed: %v", err)
	}
	_, ok, err = repo.PendingRestartRequest(ctx)
	if err != nil {
		t.Fatalf("PendingRestartRequest() after complete failed: %v", err)
	}
	if ok {
		t.Fatal("completed restart request should not remain pending")
	}
}

func TestRepositorySchemaInitializationIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.sqlite3")
	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate() failed: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() after migration failed: %v", err)
	}
	defer reopened.Close()
	version, err := reopened.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion() failed: %v", err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, currentSchemaVersion)
	}
}

func TestRepositoryTransactionRollbackOnFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	errBoom := errors.New("boom")
	err := repo.withTx(ctx, func(q *storagedb.Queries) error {
		if err := q.SetConfigValue(ctx, storagedb.SetConfigValueParams{
			Key:       "command_prefix",
			Value:     "!rollback",
			UpdatedAt: formatTime(time.Now().UTC()),
		}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("withTx() error = %v, want boom", err)
	}
	_, ok, err := repo.ConfigValue(ctx, "command_prefix")
	if err != nil {
		t.Fatalf("ConfigValue() failed: %v", err)
	}
	if ok {
		t.Fatal("config row should have rolled back")
	}
}

func TestRepositoryCleansProcessedMessages(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }
	for i := int64(1); i <= 5; i++ {
		if err := repo.MarkMessageProcessed(ctx, i); err != nil {
			t.Fatalf("MarkMessageProcessed(%d) failed: %v", i, err)
		}
	}
	repo.now = func() time.Time { return base.Add(48 * time.Hour) }
	for i := int64(6); i <= 8; i++ {
		if err := repo.MarkMessageProcessed(ctx, i); err != nil {
			t.Fatalf("MarkMessageProcessed(%d) failed: %v", i, err)
		}
	}

	deleted, err := repo.CleanupProcessedMessages(ctx, 24*time.Hour, 2)
	if err != nil {
		t.Fatalf("CleanupProcessedMessages() failed: %v", err)
	}
	if deleted != 6 {
		t.Fatalf("deleted = %d, want 6", deleted)
	}
	count, err := repo.ProcessedMessageCount(ctx)
	if err != nil {
		t.Fatalf("ProcessedMessageCount() failed: %v", err)
	}
	if count != 2 {
		t.Fatalf("processed message count = %d, want 2", count)
	}
	for _, messageID := range []int64{7, 8} {
		processed, err := repo.MessageProcessed(ctx, messageID)
		if err != nil {
			t.Fatalf("MessageProcessed(%d) failed: %v", messageID, err)
		}
		if !processed {
			t.Fatalf("message %d should remain processed", messageID)
		}
	}
}

func TestRepositoryOwnerRoleIsRejectedBySetUserRole(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	err := repo.SetUserRole(ctx, 10, permissions.RoleOwner, 0)
	if err == nil {
		t.Fatal("SetUserRole with RoleOwner should have failed")
	}
}

func TestRepositorySetUserRoleNoneDeletesLocalRole(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	if err := repo.SetUserRole(ctx, 10, permissions.RoleAdmin, 99); err != nil {
		t.Fatalf("SetUserRole(admin) failed: %v", err)
	}
	if err := repo.SetUserRole(ctx, 10, permissions.RoleNone, 99); err != nil {
		t.Fatalf("SetUserRole(none) failed: %v", err)
	}

	role, ok, err := repo.UserRole(ctx, 10)
	if err != nil {
		t.Fatalf("UserRole() failed: %v", err)
	}
	if ok {
		t.Fatalf("UserRole() found role %q after setting none; want no local row", role)
	}

	records, err := repo.ListUserRoles(ctx)
	if err != nil {
		t.Fatalf("ListUserRoles() failed: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("ListUserRoles() len = %d, want 0 after deleting local role", len(records))
	}
}

func TestRepositoryStoresAndCleansRawEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	// Store some raw events.
	for i := int64(1); i <= 3; i++ {
		if err := repo.StoreRawEvent(ctx, RawEvent{
			QueueID:    "q1",
			EventID:    i,
			EventType:  "message",
			ReceivedAt: base,
			RawJSON:    []byte(`{"type":"message"}`),
		}); err != nil {
			t.Fatalf("StoreRawEvent(%d) failed: %v", i, err)
		}
	}

	count, err := repo.RawEventCount(ctx)
	if err != nil {
		t.Fatalf("RawEventCount() failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3", count)
	}

	// Duplicate insert should be silently ignored (INSERT OR IGNORE).
	if err := repo.StoreRawEvent(ctx, RawEvent{
		QueueID:    "q1",
		EventID:    1,
		EventType:  "message",
		ReceivedAt: base,
		RawJSON:    []byte(`{"type":"message"}`),
	}); err != nil {
		t.Fatalf("duplicate StoreRawEvent failed: %v", err)
	}
	count, err = repo.RawEventCount(ctx)
	if err != nil {
		t.Fatalf("RawEventCount() after duplicate failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("count after duplicate = %d, want 3 (should be idempotent)", count)
	}

	// Add newer events.
	repo.now = func() time.Time { return base.Add(48 * time.Hour) }
	for i := int64(4); i <= 5; i++ {
		if err := repo.StoreRawEvent(ctx, RawEvent{
			QueueID:    "q1",
			EventID:    i,
			EventType:  "heartbeat",
			ReceivedAt: base.Add(48 * time.Hour),
			RawJSON:    []byte(`{"type":"heartbeat"}`),
		}); err != nil {
			t.Fatalf("StoreRawEvent(%d) failed: %v", i, err)
		}
	}

	// Cleanup with 24h retention and max 1 row.
	// Retention removes the 3 old events (at base); max 1 row trims 1 more of the 2 newer events.
	deleted, err := repo.CleanupRawEvents(ctx, 24*time.Hour, 1)
	if err != nil {
		t.Fatalf("CleanupRawEvents() failed: %v", err)
	}
	if deleted != 4 {
		t.Fatalf("deleted = %d, want 4 (3 old + 1 trimmed by maxRows)", deleted)
	}

	count, err = repo.RawEventCount(ctx)
	if err != nil {
		t.Fatalf("RawEventCount() after cleanup failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("count after cleanup = %d, want 1", count)
	}
}

func openTestRepository(t *testing.T) *Repository {
	t.Helper()

	repo, err := Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}

// ---- Channel Lifecycle Queue tests ----

func TestRepositoryStoreRawEventAndEnqueueLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	channelID := int64(42)
	err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
		QueueID:    "q1",
		EventID:    10,
		EventType:  "stream",
		ReceivedAt: base,
		RawJSON:    []byte(`{"type":"stream","id":10}`),
	}, []ChannelLifecycleEnqueueItem{
		{
			LifecycleKind: LifecycleKindChannelUpdated,
			ChannelID:     &channelID,
			ChannelName:   ptrString("my-channel"),
			Op:            "update",
		},
	})
	if err != nil {
		t.Fatalf("StoreRawEventAndEnqueueLifecycle() failed: %v", err)
	}

	rawCount, err := repo.RawEventCount(ctx)
	if err != nil {
		t.Fatalf("RawEventCount() failed: %v", err)
	}
	if rawCount != 1 {
		t.Fatalf("raw event count = %d, want 1", rawCount)
	}

	lifecycleCount, err := repo.ChannelLifecycleQueueEntryCount(ctx)
	if err != nil {
		t.Fatalf("ChannelLifecycleQueueEntryCount() failed: %v", err)
	}
	if lifecycleCount != 1 {
		t.Fatalf("lifecycle entry count = %d, want 1", lifecycleCount)
	}

	// Verify entry fields.
	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.ZulipEventID != 10 {
		t.Errorf("ZulipEventID = %d, want 10", e.ZulipEventID)
	}
	if e.LifecycleKind != LifecycleKindChannelUpdated {
		t.Errorf("LifecycleKind = %q, want %q", e.LifecycleKind, LifecycleKindChannelUpdated)
	}
	if e.ChannelID == nil || *e.ChannelID != 42 {
		t.Errorf("ChannelID = %v, want 42", e.ChannelID)
	}
	if e.ChannelName == nil || *e.ChannelName != "my-channel" {
		t.Errorf("ChannelName = %v, want \"my-channel\"", e.ChannelName)
	}
	if e.Status != LifecycleStatusPending {
		t.Errorf("Status = %q, want %q", e.Status, LifecycleStatusPending)
	}
}

func TestRepositoryChannelLifecycleQueuePendingEntriesInOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)

	// Insert 3 entries with different available_at times.
	for i, offset := range []time.Duration{2 * time.Minute, 0, time.Minute} {
		repo.now = func() time.Time { return base.Add(offset) }
		err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
			QueueID:   "q1",
			EventID:   int64(i + 1),
			EventType: "stream",
			ReceivedAt: base,
			RawJSON:   []byte(`{"type":"stream"}`),
		}, []ChannelLifecycleEnqueueItem{{
			LifecycleKind: LifecycleKindChannelCreated,
			Op:            "create",
		}})
		if err != nil {
			t.Fatalf("StoreRawEventAndEnqueueLifecycle(%d) failed: %v", i, err)
		}
	}

	// List entries up to now = base + 3 minutes.
	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(3*time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries len = %d, want 3", len(entries))
	}
	// Should be ordered by available_at ASC: 0, 1min, 2min.
	if entries[0].ZulipEventID != 2 {
		t.Errorf("entries[0].ZulipEventID = %d, want 2 (available_at=base+0)", entries[0].ZulipEventID)
	}
	if entries[1].ZulipEventID != 3 {
		t.Errorf("entries[1].ZulipEventID = %d, want 3 (available_at=base+1m)", entries[1].ZulipEventID)
	}
	if entries[2].ZulipEventID != 1 {
		t.Errorf("entries[2].ZulipEventID = %d, want 1 (available_at=base+2m)", entries[2].ZulipEventID)
	}
}

func TestRepositoryChannelLifecycleQueueClaimAndMarkDone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
		QueueID: "q1", EventID: 1, EventType: "stream",
		ReceivedAt: base, RawJSON: []byte(`{}`),
	}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
		t.Fatalf("StoreRawEventAndEnqueueLifecycle() failed: %v", err)
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("pending entries = %d, want 1", len(entries))
	}
	id := entries[0].ID

	// Claim it.
	claimed, err := repo.ClaimChannelLifecycleEntry(ctx, id, "worker-1")
	if err != nil {
		t.Fatalf("ClaimChannelLifecycleEntry() failed: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimChannelLifecycleEntry() = false, want true")
	}

	// Should no longer be pending.
	pending, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() after claim failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending entries after claim = %d, want 0", len(pending))
	}

	// Verify entry is in processing state.
	entry, ok, err := repo.GetChannelLifecycleEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetChannelLifecycleEntry() failed: %v", err)
	}
	if !ok {
		t.Fatal("GetChannelLifecycleEntry() not found")
	}
	if entry.Status != LifecycleStatusProcessing {
		t.Errorf("status = %q, want %q", entry.Status, LifecycleStatusProcessing)
	}
	if entry.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", entry.Attempts)
	}
	if entry.LockedBy != "worker-1" {
		t.Errorf("locked_by = %q, want %q", entry.LockedBy, "worker-1")
	}

	// Mark done.
	if err := repo.MarkChannelLifecycleEntryDone(ctx, id); err != nil {
		t.Fatalf("MarkChannelLifecycleEntryDone() failed: %v", err)
	}
	entry, _, err = repo.GetChannelLifecycleEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetChannelLifecycleEntry() after done failed: %v", err)
	}
	if entry.Status != LifecycleStatusDone {
		t.Errorf("status = %q, want %q", entry.Status, LifecycleStatusDone)
	}
	if entry.LockedAt != nil {
		t.Errorf("locked_at should be nil after done, got %v", entry.LockedAt)
	}
}

func TestRepositoryChannelLifecycleQueueMarkFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
		QueueID: "q1", EventID: 1, EventType: "stream",
		ReceivedAt: base, RawJSON: []byte(`{}`),
	}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
		t.Fatalf("StoreRawEventAndEnqueueLifecycle() failed: %v", err)
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	id := entries[0].ID

	if _, err := repo.ClaimChannelLifecycleEntry(ctx, id, "worker-1"); err != nil {
		t.Fatalf("ClaimChannelLifecycleEntry() failed: %v", err)
	}

	if err := repo.MarkChannelLifecycleEntryFailed(ctx, id, "some error message"); err != nil {
		t.Fatalf("MarkChannelLifecycleEntryFailed() failed: %v", err)
	}

	entry, _, err := repo.GetChannelLifecycleEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetChannelLifecycleEntry() failed: %v", err)
	}
	if entry.Status != LifecycleStatusFailed {
		t.Errorf("status = %q, want %q", entry.Status, LifecycleStatusFailed)
	}
	if entry.LastError != "some error message" {
		t.Errorf("last_error = %q, want %q", entry.LastError, "some error message")
	}
}

func TestRepositoryChannelLifecycleQueueResetEntry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
		QueueID: "q1", EventID: 1, EventType: "stream",
		ReceivedAt: base, RawJSON: []byte(`{}`),
	}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
		t.Fatalf("StoreRawEventAndEnqueueLifecycle() failed: %v", err)
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	id := entries[0].ID

	if _, err := repo.ClaimChannelLifecycleEntry(ctx, id, "worker-1"); err != nil {
		t.Fatalf("ClaimChannelLifecycleEntry() failed: %v", err)
	}
	if err := repo.MarkChannelLifecycleEntryFailed(ctx, id, "oops"); err != nil {
		t.Fatalf("MarkChannelLifecycleEntryFailed() failed: %v", err)
	}

	// Reset single entry.
	ok, err := repo.ResetChannelLifecycleEntryToPending(ctx, id)
	if err != nil {
		t.Fatalf("ResetChannelLifecycleEntryToPending() failed: %v", err)
	}
	if !ok {
		t.Fatal("ResetChannelLifecycleEntryToPending() = false, want true")
	}

	entry, _, err := repo.GetChannelLifecycleEntry(ctx, id)
	if err != nil {
		t.Fatalf("GetChannelLifecycleEntry() failed: %v", err)
	}
	if entry.Status != LifecycleStatusPending {
		t.Errorf("status = %q, want %q", entry.Status, LifecycleStatusPending)
	}
	if entry.LastError != "" {
		t.Errorf("last_error = %q, want empty after reset", entry.LastError)
	}

	// Should appear in pending list again.
	pending, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() after reset failed: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending entries after reset = %d, want 1", len(pending))
	}
}

func TestRepositoryChannelLifecycleQueueResetAllFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	// Insert 3 entries and fail all of them.
	for i := int64(1); i <= 3; i++ {
		if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
			QueueID: "q1", EventID: i, EventType: "stream",
			ReceivedAt: base, RawJSON: []byte(`{}`),
		}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
			t.Fatalf("StoreRawEventAndEnqueueLifecycle(%d) failed: %v", i, err)
		}
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	for _, e := range entries {
		if _, claimErr := repo.ClaimChannelLifecycleEntry(ctx, e.ID, "w"); claimErr != nil {
			t.Fatalf("ClaimChannelLifecycleEntry(%d) failed: %v", e.ID, claimErr)
		}
		if failErr := repo.MarkChannelLifecycleEntryFailed(ctx, e.ID, "err"); failErr != nil {
			t.Fatalf("MarkChannelLifecycleEntryFailed(%d) failed: %v", e.ID, failErr)
		}
	}

	count, err := repo.ResetAllFailedChannelLifecycleEntries(ctx)
	if err != nil {
		t.Fatalf("ResetAllFailedChannelLifecycleEntries() failed: %v", err)
	}
	if count != 3 {
		t.Fatalf("reset count = %d, want 3", count)
	}

	pending, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() after reset failed: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("pending entries after reset = %d, want 3", len(pending))
	}
}

func TestRepositoryChannelLifecycleQueueDeduplication(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	item := []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}

	// Insert same event+kind twice (INSERT OR IGNORE should make second a no-op).
	for i := 0; i < 2; i++ {
		if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
			QueueID: "q1", EventID: 1, EventType: "stream",
			ReceivedAt: base, RawJSON: []byte(`{}`),
		}, item); err != nil {
			t.Fatalf("StoreRawEventAndEnqueueLifecycle(%d) failed: %v", i, err)
		}
	}

	count, err := repo.ChannelLifecycleQueueEntryCount(ctx)
	if err != nil {
		t.Fatalf("ChannelLifecycleQueueEntryCount() failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("lifecycle entry count = %d, want 1 (deduplicated)", count)
	}
}

func TestRepositoryChannelLifecycleQueueStatusCounts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	// Insert 3 entries: mark 1 done, 1 failed, leave 1 pending.
	for i := int64(1); i <= 3; i++ {
		if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
			QueueID: "q1", EventID: i, EventType: "stream",
			ReceivedAt: base, RawJSON: []byte(`{}`),
		}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
			t.Fatalf("StoreRawEventAndEnqueueLifecycle(%d) failed: %v", i, err)
		}
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	// Claim and mark entry 0 as done.
	if _, claimErr := repo.ClaimChannelLifecycleEntry(ctx, entries[0].ID, "w"); claimErr != nil {
		t.Fatalf("claim failed: %v", claimErr)
	}
	if doneErr := repo.MarkChannelLifecycleEntryDone(ctx, entries[0].ID); doneErr != nil {
		t.Fatalf("mark done failed: %v", doneErr)
	}
	// Claim and mark entry 1 as failed.
	if _, claimErr := repo.ClaimChannelLifecycleEntry(ctx, entries[1].ID, "w"); claimErr != nil {
		t.Fatalf("claim failed: %v", claimErr)
	}
	if failErr := repo.MarkChannelLifecycleEntryFailed(ctx, entries[1].ID, "error"); failErr != nil {
		t.Fatalf("mark failed failed: %v", failErr)
	}
	// entry 2 remains pending.

	counts, err := repo.ChannelLifecycleQueueStatusCounts(ctx)
	if err != nil {
		t.Fatalf("ChannelLifecycleQueueStatusCounts() failed: %v", err)
	}

	statusMap := make(map[LifecycleStatus]int)
	for _, c := range counts {
		statusMap[c.Status] = c.Count
	}
	if statusMap[LifecycleStatusPending] != 1 {
		t.Errorf("pending count = %d, want 1", statusMap[LifecycleStatusPending])
	}
	if statusMap[LifecycleStatusDone] != 1 {
		t.Errorf("done count = %d, want 1", statusMap[LifecycleStatusDone])
	}
	if statusMap[LifecycleStatusFailed] != 1 {
		t.Errorf("failed count = %d, want 1", statusMap[LifecycleStatusFailed])
	}
}

func TestRepositoryChannelLifecycleQueueCleanup(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	base := time.Date(2026, 5, 25, 12, 0, 0, 0, time.UTC)
	repo.now = func() time.Time { return base }

	// Insert 4 entries: mark 2 done/skipped (old), leave 2 pending.
	for i := int64(1); i <= 4; i++ {
		if err := repo.StoreRawEventAndEnqueueLifecycle(ctx, RawEvent{
			QueueID: "q1", EventID: i, EventType: "stream",
			ReceivedAt: base, RawJSON: []byte(`{}`),
		}, []ChannelLifecycleEnqueueItem{{LifecycleKind: LifecycleKindChannelCreated, Op: "create"}}); err != nil {
			t.Fatalf("StoreRawEventAndEnqueueLifecycle(%d) failed: %v", i, err)
		}
	}

	entries, err := repo.ListPendingChannelLifecycleEntries(ctx, base.Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("ListPendingChannelLifecycleEntries() failed: %v", err)
	}
	// entries[0]: done, entries[1]: skipped (both at base time)
	if _, claimErr := repo.ClaimChannelLifecycleEntry(ctx, entries[0].ID, "w"); claimErr != nil {
		t.Fatalf("claim failed: %v", claimErr)
	}
	if doneErr := repo.MarkChannelLifecycleEntryDone(ctx, entries[0].ID); doneErr != nil {
		t.Fatalf("mark done failed: %v", doneErr)
	}
	if _, claimErr := repo.ClaimChannelLifecycleEntry(ctx, entries[1].ID, "w"); claimErr != nil {
		t.Fatalf("claim failed: %v", claimErr)
	}
	if skipErr := repo.MarkChannelLifecycleEntrySkipped(ctx, entries[1].ID); skipErr != nil {
		t.Fatalf("mark skipped failed: %v", skipErr)
	}

	// Now advance time by 48h and cleanup with 24h retention.
	repo.now = func() time.Time { return base.Add(48 * time.Hour) }
	deleted, err := repo.CleanupChannelLifecycleEntries(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("CleanupChannelLifecycleEntries() failed: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	total, err := repo.ChannelLifecycleQueueEntryCount(ctx)
	if err != nil {
		t.Fatalf("ChannelLifecycleQueueEntryCount() after cleanup failed: %v", err)
	}
	if total != 2 {
		t.Fatalf("total entries after cleanup = %d, want 2 (pending ones remain)", total)
	}
}

func ptrString(s string) *string {
	return &s
}
