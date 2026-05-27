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
	channelMentionPattern    = regexp.MustCompile(`^#\*\*(.+)\*\*$`)
	renderedUserIDPattern    = regexp.MustCompile(`data-user-id="(\d+)"`)
	renderedChannelIDPattern = regexp.MustCompile(`data-stream-id="(\d+)"`)
)

func zulipUserMentionName(token string) (string, bool) {
	matches := userMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return "", false
	}
	return matches[1], true
}

func zulipUserMentionNameAndID(token string) (string, int64, bool) {
	matches := userMentionWithIDPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return "", 0, false
	}
	id, err := strconv.ParseInt(matches[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return matches[1], id, true
}

func zulipChannelMentionName(token string) (string, bool) {
	matches := channelMentionPattern.FindStringSubmatch(strings.TrimSpace(token))
	if matches == nil {
		return "", false
	}
	return matches[1], true
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
