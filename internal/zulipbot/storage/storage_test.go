package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
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

func openTestRepository(t *testing.T) *Repository {
	t.Helper()

	repo, err := Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
