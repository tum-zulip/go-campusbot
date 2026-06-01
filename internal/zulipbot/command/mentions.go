package command

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var errRenderedIDNotFound = errors.New("rendered mention did not contain an ID")

var (
	userMentionPattern       = regexp.MustCompile(`^@_?\*\*([^\*` + "`" + `\\>"@]+)\*\*$`)
	userMentionWithIDPattern = regexp.MustCompile(`^@_?\*\*([^\*` + "`" + `\\>"@]+)\|(\d+)\*\*$`)
	userGroupMentionPattern  = regexp.MustCompile(`^@_?\*([^\*` + "`" + `\\>"@]+)\*$`)
	channelMentionPattern    = regexp.MustCompile(`^#_?\*\*(.+)\*\*$`)
	renderedUserIDPattern    = regexp.MustCompile(`data-user-id="(\d+)"`)
	renderedGroupIDPattern   = regexp.MustCompile(`data-user-group-id="(\d+)"`)
	renderedChannelIDPattern = regexp.MustCompile(`data-stream-id="(\d+)"`)
)

func isZulipUserMention(token string) bool {
	matches := userMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	return matches != nil
}

func zulipUserMentionID(token string) (int64, bool) {
	matches := userMentionWithIDPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func zulipUserMentionName(token string) (string, bool) {
	matches := userMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return "", false
	}
	name, _, _ := strings.Cut(matches[1], "|")
	return name, name != ""
}

func zulipUserGroupMentionName(token string) (string, bool) {
	matches := userGroupMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return "", false
	}
	return matches[1], true
}

func isZulipChannelMention(token string) bool {
	matches := channelMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	return matches != nil
}

func idFromRenderedHTML(pattern *regexp.Regexp, html string) (int64, error) {
	matches := pattern.FindStringSubmatch(html)
	if matches == nil {
		return 0, errRenderedIDNotFound
	}
	id, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse rendered mention ID %q: %w", matches[1], err)
	}
	return id, nil
}
