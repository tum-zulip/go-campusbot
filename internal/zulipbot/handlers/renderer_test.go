package handlers_test

import (
	"strings"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
)

func rendererMapping(shortName, emojiName string, channelGroupID int64) storagedb.EmojiGroupMapping {
	return storagedb.EmojiGroupMapping{
		ID:             1,
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        1,
		SortOrder:      0,
		CreatedAt:      "2026-05-27T00:00:00Z",
		UpdatedAt:      "2026-05-27T00:00:00Z",
	}
}

func TestRenderEmpty(t *testing.T) {
	t.Parallel()
	content := handlers.RenderAnnouncement(nil)
	if !strings.Contains(content, "Hi!") {
		t.Error("expected preamble in rendered content")
	}
	if !strings.Contains(content, "Have a nice day") {
		t.Error("expected postamble in rendered content")
	}
	if !strings.Contains(content, "| Course | Emoji |") {
		t.Error("expected table header in rendered content")
	}
}

func TestRenderSingleMapping(t *testing.T) {
	t.Parallel()
	mappings := []storagedb.EmojiGroupMapping{
		rendererMapping("WI", "wi", 42),
	}
	content := handlers.RenderAnnouncement(mappings)
	if !strings.Contains(content, "WI") {
		t.Error("expected short name in rendered content")
	}
	if !strings.Contains(content, ":wi:") {
		t.Error("expected emoji in rendered content")
	}
}

func TestRenderThreeMappings(t *testing.T) {
	t.Parallel()
	mappings := []storagedb.EmojiGroupMapping{
		rendererMapping("A", "alpha", 1),
		rendererMapping("B", "beta", 2),
		rendererMapping("C", "gamma", 3),
	}
	content := handlers.RenderAnnouncement(mappings)
	lines := strings.Split(content, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "A") && strings.Contains(line, "B") && strings.Contains(line, "C") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected all three mappings in one row; lines:\n%s", strings.Join(lines, "\n"))
	}
}

func TestRenderFourMappingsPadsToSix(t *testing.T) {
	t.Parallel()
	mappings := []storagedb.EmojiGroupMapping{
		rendererMapping("A", "alpha", 1),
		rendererMapping("B", "beta", 2),
		rendererMapping("C", "gamma", 3),
		rendererMapping("D", "delta", 4),
	}
	content := handlers.RenderAnnouncement(mappings)
	if !strings.Contains(content, "A") || !strings.Contains(content, "D") {
		t.Error("expected all four mappings in rendered content")
	}
	lines := strings.Split(content, "\n")
	dataRows := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "|") && !strings.Contains(line, "---") && !strings.Contains(line, "Course") {
			dataRows++
		}
	}
	if dataRows != 2 {
		t.Errorf("expected 2 data rows, got %d", dataRows)
	}
}

func TestRenderMarkdownEscaping(t *testing.T) {
	t.Parallel()
	mappings := []storagedb.EmojiGroupMapping{
		rendererMapping("Course|With|Pipes", "emoji", 1),
	}
	content := handlers.RenderAnnouncement(mappings)
	if strings.Contains(content, "Course|With|Pipes") {
		t.Error("expected pipes to be escaped in short name")
	}
	if !strings.Contains(content, `Course\|With\|Pipes`) {
		t.Error("expected escaped pipes in short name")
	}
}

func TestContentHashDeterministic(t *testing.T) {
	t.Parallel()
	mappings := []storagedb.EmojiGroupMapping{
		rendererMapping("WI", "wi", 42),
	}
	hash1 := handlers.AnnouncementContentHash(mappings)
	hash2 := handlers.AnnouncementContentHash(mappings)
	if hash1 != hash2 {
		t.Errorf("ContentHash not deterministic: %s != %s", hash1, hash2)
	}
	if len(hash1) != 64 {
		t.Errorf("expected 64-char SHA256 hex, got len=%d", len(hash1))
	}
}

func TestContentHashChangesWithMappings(t *testing.T) {
	t.Parallel()
	empty := handlers.AnnouncementContentHash(nil)
	one := handlers.AnnouncementContentHash([]storagedb.EmojiGroupMapping{rendererMapping("WI", "wi", 42)})
	if empty == one {
		t.Error("expected different hashes for empty and non-empty mappings")
	}
}
