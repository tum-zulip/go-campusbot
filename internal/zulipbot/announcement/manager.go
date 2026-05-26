package announcement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

// Sender is the interface the Manager needs to send/edit messages and add reactions.
type Sender interface {
	OwnUserID() int64
	SendChannelMessage(ctx context.Context, channelID int64, topic string, content string) (int64, error)
	EditMessage(ctx context.Context, messageID int64, content string) error
	AddReaction(ctx context.Context, messageID int64, emojiName, emojiCode, reactionType string) error
}

// Manager manages the announcement message lifecycle.
type Manager struct {
	repo   *storage.Repository
	sender Sender
	logger *slog.Logger
}

// NewManager creates a new announcement Manager.
func NewManager(repo *storage.Repository, sender Sender, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		repo:   repo,
		sender: sender,
		logger: logger,
	}
}

// SendParams provides channel/topic for sending a new announcement message.
// Only required when no message_id is already stored in announcement_state.
type SendParams struct {
	ChannelID int64
	Topic     string
}

// EnsureAnnouncement sends or edits the announcement message.
//   - If no message_id stored: send new message to configured channel/topic, store message_id.
//   - If message_id stored: edit the message if content has changed.
//   - After send/edit: add bot reactions for all enabled emoji mappings.
func (m *Manager) EnsureAnnouncement(ctx context.Context, send *SendParams) error {
	mappings, err := m.repo.ListEnabledEmojiGroupMappings(ctx)
	if err != nil {
		return fmt.Errorf("list emoji group mappings: %w", err)
	}

	content := Render(mappings)
	hash := ContentHash(mappings)

	state, ok, err := m.repo.GetAnnouncementState(ctx)
	if err != nil {
		return fmt.Errorf("get announcement state: %w", err)
	}

	var messageID int64
	if !ok || state.MessageID == nil {
		// Send new message — send params required
		if send == nil || send.ChannelID <= 0 || send.Topic == "" {
			return errors.New("no announcement message_id stored and no channel/topic provided: " +
				"run `group announce set-message <id>` to migrate from an existing message, " +
				"or set announcement.channel_id and announcement.topic to create a new one",
			)
		}
		msgID, err := m.sender.SendChannelMessage(ctx, send.ChannelID, send.Topic, content)
		if err != nil {
			return fmt.Errorf("send announcement message: %w", err)
		}
		messageID = msgID
		newState := storage.AnnouncementState{
			MessageID:   &messageID,
			ContentHash: hash,
		}
		if err := m.repo.SaveAnnouncementState(ctx, newState); err != nil {
			return fmt.Errorf("save announcement state: %w", err)
		}
		m.logger.InfoContext(ctx, "sent new announcement message", "message_id", messageID)
	} else {
		messageID = *state.MessageID
		if state.ContentHash != hash {
			if err := m.sender.EditMessage(ctx, messageID, content); err != nil {
				return fmt.Errorf("edit announcement message: %w", err)
			}
			newState := storage.AnnouncementState{
				MessageID:   &messageID,
				ContentHash: hash,
			}
			if err := m.repo.SaveAnnouncementState(ctx, newState); err != nil {
				return fmt.Errorf("save announcement state: %w", err)
			}
			m.logger.InfoContext(ctx, "updated announcement message", "message_id", messageID)
		} else {
			m.logger.InfoContext(ctx, "announcement message is up to date", "message_id", messageID)
		}
	}

	// Add bot reactions for all enabled emoji mappings
	m.addReactions(ctx, messageID, mappings)

	return nil
}

// UpdateAfterMappingChange re-renders and edits the announcement if mappings changed.
func (m *Manager) UpdateAfterMappingChange(ctx context.Context, send *SendParams) error {
	return m.EnsureAnnouncement(ctx, send)
}

// addReactions adds the bot's reactions for each enabled mapping.
// Errors are logged but not propagated (already-reacted is non-fatal).
func (m *Manager) addReactions(ctx context.Context, messageID int64, mappings []storage.EmojiGroupMapping) {
	for _, mapping := range mappings {
		if err := m.sender.AddReaction(ctx, messageID, mapping.EmojiName, mapping.EmojiCode, mapping.ReactionType); err != nil {
			m.logger.WarnContext(ctx, "failed to add bot reaction to announcement",
				"message_id", messageID,
				"emoji_name", mapping.EmojiName,
				"error", err)
		}
	}
}
