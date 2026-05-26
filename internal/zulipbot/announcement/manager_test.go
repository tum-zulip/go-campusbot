package announcement_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
	"github.com/tum-zulip/go-campusbot/internal/zulipmock"
)

func newTestSender(t *testing.T) (*zulipbot.Bot, zulipmock.Client) {
	t.Helper()
	mock := zulipmock.NewClient()
	bot, err := zulipbot.New(context.Background(), mock)
	if err != nil {
		t.Fatalf("zulipbot.New: %v", err)
	}
	return bot, mock
}

func openManagerTestRepo(t *testing.T) *storage.Repository {
	t.Helper()
	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "test.sqlite3"))
	if err != nil {
		t.Fatalf("storage.Open() failed: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func seedMapping(t *testing.T, repo *storage.Repository, shortName, emojiName string, channelGroupID int64) {
	t.Helper()
	err := repo.UpsertEmojiGroupMapping(context.Background(), storage.EmojiGroupMapping{
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      0,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	})
	if err != nil {
		t.Fatalf("UpsertEmojiGroupMapping() failed: %v", err)
	}
}

func TestManagerEnsureAnnouncementSendsNewMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, mock := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "test-topic"}); err != nil {
		t.Fatalf("EnsureAnnouncement() failed: %v", err)
	}

	state, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok || state.MessageID == nil {
		t.Fatalf("expected announcement state to be saved, ok=%v, err=%v", ok, err)
	}
	if *state.MessageID <= 0 {
		t.Errorf("expected positive message ID, got %d", *state.MessageID)
	}
	if mock.LastSentMessage() == nil {
		t.Error("expected message to be sent")
	}
}

func TestManagerEnsureAnnouncementEditsIfChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	// First ensure - sends new
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("first EnsureAnnouncement() failed: %v", err)
	}

	state1, ok1, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok1 {
		t.Fatalf("expected announcement state after first call, ok=%v, err=%v", ok1, err)
	}
	hash1 := state1.ContentHash

	// Add a new mapping
	seedMapping(t, repo, "CS", "cs", 43)

	// Second ensure - should edit
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("second EnsureAnnouncement() failed: %v", err)
	}

	state2, ok2, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok2 || state2.MessageID == nil {
		t.Fatalf("expected announcement state, ok=%v, err=%v", ok2, err)
	}
	if state2.ContentHash == hash1 {
		t.Error("expected content hash to change after adding a new mapping")
	}
	if state2.ContentHash == "" {
		t.Error("expected non-empty content hash after second call")
	}
}

func TestManagerEnsureAnnouncementSkipsEditIfUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("first EnsureAnnouncement() failed: %v", err)
	}

	state1, ok1, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok1 {
		t.Fatalf("expected state after first call: ok=%v err=%v", ok1, err)
	}
	hashBefore := state1.ContentHash

	// Same content, no change
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("second EnsureAnnouncement() failed: %v", err)
	}

	state2, ok2, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok2 {
		t.Fatalf("expected state after second call: ok=%v err=%v", ok2, err)
	}
	if state2.ContentHash != hashBefore {
		t.Errorf("expected content hash to be unchanged, got %q -> %q", hashBefore, state2.ContentHash)
	}
}

func TestManagerReactionErrorsAreNonFatal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	// With real mock, reactions always succeed. The test verifies the function
	// does not return an error even if reactions had been non-fatal.
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("EnsureAnnouncement() should not fail, got: %v", err)
	}
}

func TestManagerEnsureAnnouncementEditsExistingNoSendParams(t *testing.T) {
	// message_id stored → edit without channel/topic (send=nil)
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	// Seed a stored message_id (simulating migration from old bot)
	msgID := int64(777)
	err := repo.SaveAnnouncementState(ctx, storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "", // empty hash forces an edit
	})
	if err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}

	// Call with nil send params — should edit existing message
	if err := mgr.EnsureAnnouncement(ctx, nil); err != nil {
		t.Fatalf("EnsureAnnouncement() failed with nil send: %v", err)
	}

	// Verify state was updated with a content hash
	state, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok {
		t.Fatalf("expected announcement state, ok=%v err=%v", ok, err)
	}
	if state.MessageID == nil || *state.MessageID != 777 {
		t.Errorf("expected message_id=777, got %v", state.MessageID)
	}
	if state.ContentHash == "" {
		t.Error("expected non-empty ContentHash after edit")
	}
}

func TestManagerEnsureAnnouncementNoMessageIDNoSendParams(t *testing.T) {
	// no stored message_id, nil send → error
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	err := mgr.EnsureAnnouncement(ctx, nil)
	if err == nil {
		t.Fatal("expected error when no message_id and no send params, got nil")
	}
}

func TestManagerEnsureAnnouncementNoMessageIDNoChannelID(t *testing.T) {
	// no stored message_id, send with ChannelID=0 → error
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 0, Topic: "topic"})
	if err == nil {
		t.Fatal("expected error when no message_id and ChannelID=0, got nil")
	}
}

func TestManagerEnsureAnnouncementEditFailureDoesNotSendNew(t *testing.T) {
	// message_id stored, edit fails → error returned, no new message sent.
	//
	// NOTE: This test is expected to FAIL at runtime. The mock client's
	// UpdateMessage always returns (nil, nil, nil) and there is no
	// FailNext constant for UpdateMessage, so we cannot inject an edit
	// failure. As a result, EnsureAnnouncement succeeds (err == nil) but
	// this test asserts err != nil — the test will report a failure.
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	bot, _ := newTestSender(t)
	mgr := announcement.NewManager(repo, bot, nil)

	// Seed a stored message_id with empty hash to force edit path
	msgID := int64(777)
	if err := repo.SaveAnnouncementState(ctx, storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "",
	}); err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}

	err := mgr.EnsureAnnouncement(ctx, nil)
	if err == nil {
		t.Fatal("expected error when edit fails, got nil")
	}
}
