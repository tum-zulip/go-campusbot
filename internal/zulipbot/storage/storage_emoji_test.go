package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestEmojiGroupMappingUpsertAndList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	m1 := storage.EmojiGroupMapping{
		ShortName:      "WI",
		DisplayName:    "Wirtschaftsinformatik",
		ChannelGroupID: 10,
		EmojiName:      "wi",
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      0,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := repo.UpsertEmojiGroupMapping(ctx, m1); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}

	m2 := storage.EmojiGroupMapping{
		ShortName:      "CS",
		DisplayName:    "Computer Science",
		ChannelGroupID: 20,
		EmojiName:      "cs",
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      1,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := repo.UpsertEmojiGroupMapping(ctx, m2); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}

	enabled, err := repo.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		t.Fatalf("ListEnabledEmojiGroupMappings() failed: %v", err)
	}
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled mappings, got %d", len(enabled))
	}

	all, err := repo.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		t.Fatalf("ListAllEmojiGroupMappings() failed: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 total mappings, got %d", len(all))
	}
}

func TestEmojiGroupMappingGetByShortName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	m := storage.EmojiGroupMapping{
		ShortName:      "WI",
		DisplayName:    "WI",
		ChannelGroupID: 10,
		EmojiName:      "wi",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := repo.UpsertEmojiGroupMapping(ctx, m); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}

	found, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "WI")
	if err != nil || !ok {
		t.Fatalf("GetEmojiGroupMappingByShortName() failed: err=%v, ok=%v", err, ok)
	}
	if found.ChannelGroupID != 10 {
		t.Errorf("expected ChannelGroupID 10, got %d", found.ChannelGroupID)
	}

	// Not found
	_, ok, err = repo.GetEmojiGroupMappingByShortName(ctx, "nonexistent")
	if err != nil || ok {
		t.Errorf("expected not found, got err=%v, ok=%v", err, ok)
	}
}

func TestEmojiGroupMappingGetByEmoji(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	m := storage.EmojiGroupMapping{
		ShortName:      "WI",
		DisplayName:    "WI",
		ChannelGroupID: 10,
		EmojiName:      "wi",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := repo.UpsertEmojiGroupMapping(ctx, m); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}

	found, ok, err := repo.GetEmojiGroupMappingByEmoji(ctx, "wi", "unicode_emoji")
	if err != nil || !ok {
		t.Fatalf("GetEmojiGroupMappingByEmoji() failed: err=%v, ok=%v", err, ok)
	}
	if found.ShortName != "WI" {
		t.Errorf("expected ShortName WI, got %q", found.ShortName)
	}

	// Not found
	_, ok, err = repo.GetEmojiGroupMappingByEmoji(ctx, "unknown", "unicode_emoji")
	if err != nil || ok {
		t.Errorf("expected not found, got err=%v, ok=%v", err, ok)
	}
}

func TestEmojiGroupMappingDisabledNotReturned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	m := storage.EmojiGroupMapping{
		ShortName:      "WI",
		DisplayName:    "WI",
		ChannelGroupID: 10,
		EmojiName:      "wi",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	if err := repo.UpsertEmojiGroupMapping(ctx, m); err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}
	if err := repo.SetEmojiGroupMappingEnabled(ctx, "WI", false); err != nil {
		t.Fatalf("SetEmojiGroupMappingEnabled() failed: %v", err)
	}

	enabled, err := repo.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		t.Fatalf("ListEnabledEmojiGroupMappings() failed: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("expected 0 enabled mappings after disable, got %d", len(enabled))
	}

	// ListAll should still show it
	all, err := repo.ListAllEmojiGroupMappings(ctx)
	if err != nil {
		t.Fatalf("ListAllEmojiGroupMappings() failed: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 in ListAll, got %d", len(all))
	}
	if all[0].Enabled {
		t.Error("expected mapping to be disabled")
	}

	// GetByShortName should not find disabled
	_, ok, err := repo.GetEmojiGroupMappingByShortName(ctx, "WI")
	if err != nil || ok {
		t.Errorf("expected not found for disabled mapping, got err=%v, ok=%v", err, ok)
	}
}

func TestAnnouncementState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	// Initially not found
	_, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || ok {
		t.Fatalf("expected no announcement state, got err=%v, ok=%v", err, ok)
	}

	// Save state
	msgID := int64(12345)
	state := storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "abc123",
	}
	if err := repo.SaveAnnouncementState(ctx, state); err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}

	// Read it back
	loaded, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok {
		t.Fatalf("expected announcement state, got err=%v, ok=%v", err, ok)
	}
	if loaded.MessageID == nil || *loaded.MessageID != msgID {
		t.Errorf("expected MessageID %d, got %v", msgID, loaded.MessageID)
	}
	if loaded.ContentHash != "abc123" {
		t.Errorf("expected ContentHash abc123, got %q", loaded.ContentHash)
	}

	// Update
	newHash := "def456"
	state.ContentHash = newHash
	if err := repo.SaveAnnouncementState(ctx, state); err != nil {
		t.Fatalf("second SaveAnnouncementState() failed: %v", err)
	}
	loaded, _, _ = repo.GetAnnouncementState(ctx)
	if loaded.ContentHash != newHash {
		t.Errorf("expected updated hash %q, got %q", newHash, loaded.ContentHash)
	}
}

func TestAnnouncementStateNilMessageID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	state := storage.AnnouncementState{
		MessageID:   nil,
		ContentHash: "xyz",
	}
	if err := repo.SaveAnnouncementState(ctx, state); err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}

	loaded, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok {
		t.Fatalf("expected state, got err=%v, ok=%v", err, ok)
	}
	if loaded.MessageID != nil {
		t.Errorf("expected nil MessageID, got %v", loaded.MessageID)
	}
}

func TestReactionProcessedDedup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openTestRepository(t)
	defer repo.Close()

	// Initially not processed
	processed, err := repo.IsReactionProcessed(ctx, 100, 200, "wi", "add")
	if err != nil || processed {
		t.Fatalf("expected not processed, got err=%v, processed=%v", err, processed)
	}

	// Mark as processed
	if err := repo.MarkReactionProcessed(ctx, 100, 200, "wi", "add"); err != nil {
		t.Fatalf("MarkReactionProcessed() failed: %v", err)
	}

	// Now it should be processed
	processed, err = repo.IsReactionProcessed(ctx, 100, 200, "wi", "add")
	if err != nil || !processed {
		t.Fatalf("expected processed, got err=%v, processed=%v", err, processed)
	}

	// Different op is not processed
	processed, err = repo.IsReactionProcessed(ctx, 100, 200, "wi", "remove")
	if err != nil || processed {
		t.Errorf("expected different op not processed, got err=%v, processed=%v", err, processed)
	}

	// Idempotent mark (INSERT OR IGNORE)
	if err := repo.MarkReactionProcessed(ctx, 100, 200, "wi", "add"); err != nil {
		t.Fatalf("duplicate MarkReactionProcessed() should not fail: %v", err)
	}
}
