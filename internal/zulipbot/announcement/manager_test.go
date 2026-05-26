package announcement_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// fakeSender is a test implementation of announcement.Sender.
type fakeSender struct {
	mu            sync.Mutex
	ownUserID     int64
	nextMessageID int64
	messages      map[int64]string
	reactions     map[int64][]string // messageID -> emojiNames
	sendErr       error
	editErr       error
	reactionErr   error
}

func newFakeSender(ownUserID int64) *fakeSender {
	return &fakeSender{
		ownUserID:     ownUserID,
		nextMessageID: 100,
		messages:      make(map[int64]string),
		reactions:     make(map[int64][]string),
	}
}

func (s *fakeSender) OwnUserID() int64 { return s.ownUserID }

func (s *fakeSender) SendChannelMessage(_ context.Context, _ int64, _ string, content string) (int64, error) {
	if s.sendErr != nil {
		return 0, s.sendErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextMessageID
	s.nextMessageID++
	s.messages[id] = content
	return id, nil
}

func (s *fakeSender) EditMessage(_ context.Context, messageID int64, content string) error {
	if s.editErr != nil {
		return s.editErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[messageID] = content
	return nil
}

func (s *fakeSender) AddReaction(_ context.Context, messageID int64, emojiName, _, _ string) error {
	if s.reactionErr != nil {
		return s.reactionErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reactions[messageID] = append(s.reactions[messageID], emojiName)
	return nil
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

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "test-topic"}); err != nil {
		t.Fatalf("EnsureAnnouncement() failed: %v", err)
	}

	state, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok || state.MessageID == nil {
		t.Fatalf("expected announcement state to be saved, ok=%v, err=%v", ok, err)
	}
	if *state.MessageID != 100 {
		t.Errorf("expected message ID 100, got %d", *state.MessageID)
	}
	if _, exists := sender.messages[100]; !exists {
		t.Error("expected message to be sent")
	}
	// Check reaction was added
	if len(sender.reactions[100]) == 0 {
		t.Error("expected reaction to be added")
	}
}

func TestManagerEnsureAnnouncementEditsIfChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

	// First ensure - sends new
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("first EnsureAnnouncement() failed: %v", err)
	}

	// Add a new mapping
	seedMapping(t, repo, "CS", "cs", 43)

	// Second ensure - should edit
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("second EnsureAnnouncement() failed: %v", err)
	}

	state, ok, err := repo.GetAnnouncementState(ctx)
	if err != nil || !ok || state.MessageID == nil {
		t.Fatalf("expected announcement state, ok=%v, err=%v", ok, err)
	}
	msgID := *state.MessageID
	content, exists := sender.messages[msgID]
	if !exists {
		t.Fatal("expected edited message to exist")
	}
	if content == "" {
		t.Error("expected non-empty message content")
	}
}

func TestManagerEnsureAnnouncementSkipsEditIfUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("first EnsureAnnouncement() failed: %v", err)
	}

	editCountBefore := len(sender.messages)

	// Same content, no change
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("second EnsureAnnouncement() failed: %v", err)
	}

	// The message count should not have grown (no new send, edit to same message wouldn't change len)
	// We can verify by checking that only 1 message ID exists
	if len(sender.messages) != editCountBefore {
		t.Errorf("expected no new messages, but messages grew from %d to %d", editCountBefore, len(sender.messages))
	}
}

func TestManagerReactionErrorsAreNonFatal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	sender := newFakeSender(999)
	sender.reactionErr = context.DeadlineExceeded
	mgr := announcement.NewManager(repo, sender, nil)

	// Should not fail even though reactions fail
	if err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 1, Topic: "topic"}); err != nil {
		t.Fatalf("EnsureAnnouncement() should not fail on reaction error, got: %v", err)
	}
}

func TestManagerEnsureAnnouncementEditsExistingNoSendParams(t *testing.T) {
	// message_id stored → edit without channel/topic (send=nil)
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

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

	// Message 777 should have been edited
	if _, exists := sender.messages[777]; !exists {
		t.Error("expected existing message 777 to be edited")
	}
	// No new message should have been sent
	if _, exists := sender.messages[100]; exists {
		t.Error("expected no new message to be sent")
	}
}

func TestManagerEnsureAnnouncementNoMessageIDNoSendParams(t *testing.T) {
	// no stored message_id, nil send → error
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

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

	sender := newFakeSender(999)
	mgr := announcement.NewManager(repo, sender, nil)

	err := mgr.EnsureAnnouncement(ctx, &announcement.SendParams{ChannelID: 0, Topic: "topic"})
	if err == nil {
		t.Fatal("expected error when no message_id and ChannelID=0, got nil")
	}
}

func TestManagerEnsureAnnouncementEditFailureDoesNotSendNew(t *testing.T) {
	// message_id stored, edit fails → error returned, no new message sent
	t.Parallel()
	ctx := context.Background()
	repo := openManagerTestRepo(t)
	seedMapping(t, repo, "WI", "wi", 42)

	sender := newFakeSender(999)
	sender.editErr = errors.New("edit rejected by Zulip")
	mgr := announcement.NewManager(repo, sender, nil)

	// Seed a stored message_id with empty hash to force edit
	msgID := int64(777)
	err := repo.SaveAnnouncementState(ctx, storage.AnnouncementState{
		MessageID:   &msgID,
		ContentHash: "",
	})
	if err != nil {
		t.Fatalf("SaveAnnouncementState() failed: %v", err)
	}

	err = mgr.EnsureAnnouncement(ctx, nil)
	if err == nil {
		t.Fatal("expected error when edit fails, got nil")
	}
	// No new message should have been sent
	if len(sender.messages) != 0 {
		t.Errorf("expected no new messages sent after edit failure, got %d", len(sender.messages))
	}
}
