package announcement_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func mapping(shortName, emojiName string, channelGroupID int64) storage.EmojiGroupMapping {
	return storage.EmojiGroupMapping{
		ID:             1,
		ShortName:      shortName,
		ChannelGroupID: channelGroupID,
		EmojiName:      emojiName,
		EmojiCode:      "",
		ReactionType:   "unicode_emoji",
		Enabled:        true,
		SortOrder:      0,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
}

func TestRenderEmpty(t *testing.T) {
	t.Parallel()
	content := announcement.Render(nil)
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
	mappings := []storage.EmojiGroupMapping{
		mapping("WI", "wi", 42),
	}
	content := announcement.Render(mappings)
	if !strings.Contains(content, "WI") {
		t.Error("expected short name in rendered content")
	}
	if !strings.Contains(content, ":wi:") {
		t.Error("expected emoji in rendered content")
	}
}

func TestRenderThreeMappings(t *testing.T) {
	t.Parallel()
	mappings := []storage.EmojiGroupMapping{
		mapping("A", "alpha", 1),
		mapping("B", "beta", 2),
		mapping("C", "gamma", 3),
	}
	content := announcement.Render(mappings)
	// All three should be in one row
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
	mappings := []storage.EmojiGroupMapping{
		mapping("A", "alpha", 1),
		mapping("B", "beta", 2),
		mapping("C", "gamma", 3),
		mapping("D", "delta", 4),
	}
	content := announcement.Render(mappings)
	if !strings.Contains(content, "A") || !strings.Contains(content, "D") {
		t.Error("expected all four mappings in rendered content")
	}
	// Should have two data rows (first row has A,B,C; second row has D + padding)
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
	mappings := []storage.EmojiGroupMapping{
		mapping("Course|With|Pipes", "emoji", 1),
	}
	content := announcement.Render(mappings)
	if strings.Contains(content, "Course|With|Pipes") {
		t.Error("expected pipes to be escaped in short name")
	}
	if !strings.Contains(content, `Course\|With\|Pipes`) {
		t.Error("expected escaped pipes in short name")
	}
}

func TestContentHashDeterministic(t *testing.T) {
	t.Parallel()
	mappings := []storage.EmojiGroupMapping{
		mapping("WI", "wi", 42),
	}
	hash1 := announcement.ContentHash(mappings)
	hash2 := announcement.ContentHash(mappings)
	if hash1 != hash2 {
		t.Errorf("ContentHash not deterministic: %s != %s", hash1, hash2)
	}
	if len(hash1) != 64 {
		t.Errorf("expected 64-char SHA256 hex, got len=%d", len(hash1))
	}
}

func TestContentHashChangesWithMappings(t *testing.T) {
	t.Parallel()
	empty := announcement.ContentHash(nil)
	one := announcement.ContentHash([]storage.EmojiGroupMapping{mapping("WI", "wi", 42)})
	if empty == one {
		t.Error("expected different hashes for empty and non-empty mappings")
	}
}
