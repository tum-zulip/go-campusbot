package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const preamble = `Hi! :bothappy:

I have the pleasure to announce some channel groups here.

You may subscribe to a channel group in order to be automatically subscribed to all channels belonging to that group. Also, you will be kept updated when new channels are added to the group.

Just react to this message with the emoji of the channel group you like to subscribe to. Remove your emoji to unsubscribe from this group. (1, 2)`

const postamble = `In case the emojis do not work for you, you may write me a PM:
- ` + "`group subscribe <course_short_name>`" + `
- ` + "`group unsubscribe <course_short_name>`" + `

Have a nice day! :bothappypad:

(1) Note that this will also unsubscribe you from the existing channels of this group. If you only want to cancel the subscription without being unsubscribed from existing channels, just write me a PM:
- ` + "`group unsubscribe -k <course_short_name>`" + `

(2) If your course has changed its emote, remove your reaction of the old emote and react with the new one. Then, you can remove the new reaction again to unsubscribe from the group and its channels.`

// RenderAnnouncement generates the announcement message markdown from enabled emoji→group mappings.
// The table uses a 3-column group layout (Course | Emoji pairs, 3 pairs per row).
func RenderAnnouncement(mappings []storage.EmojiGroupMapping) string {
	var b strings.Builder

	b.WriteString(preamble)
	b.WriteString("\n\n")
	b.WriteString(renderTable(mappings))
	b.WriteString("\n\n")
	b.WriteString(postamble)

	return b.String()
}

// AnnouncementContentHash returns a SHA256 hex digest of the rendered content (for change detection).
func AnnouncementContentHash(mappings []storage.EmojiGroupMapping) string {
	content := RenderAnnouncement(mappings)
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func escapeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "*", `\*`)
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

func renderTable(mappings []storage.EmojiGroupMapping) string {
	const cols = 3

	var b strings.Builder

	b.WriteString("| Course | Emoji |   | Course | Emoji |   | Course | Emoji |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- |\n")

	if len(mappings) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}

	padded := make([]storage.EmojiGroupMapping, 0, len(mappings)+cols-1)
	padded = append(padded, mappings...)
	for len(padded)%cols != 0 {
		padded = append(padded, storage.EmojiGroupMapping{})
	}

	for i := 0; i < len(padded); i += cols {
		b.WriteString("|")
		for col := range cols {
			m := padded[i+col]
			var courseName, emojiStr string
			if m.ShortName != "" {
				courseName = escapeMarkdown(m.ShortName)
				emojiStr = ":" + m.EmojiName + ":"
			}
			b.WriteString(" ")
			b.WriteString(courseName)
			b.WriteString(" | ")
			b.WriteString(emojiStr)
			b.WriteString(" |")
			if col < cols-1 {
				b.WriteString("   |")
			}
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}
