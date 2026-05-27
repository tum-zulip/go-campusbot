package zulipbot_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
)

func TestDispatchConfigSetGetList(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	ctx := context.Background()

	setResult := bot.Dispatch(ctx, ownerRequest("config", "set", zulipbot.KeyRestartStartupNotification, "yes"))
	if !strings.Contains(setResult.Content, "Configuration updated") || !strings.Contains(setResult.Content, "true") {
		t.Fatalf("set reply = %q", setResult.Content)
	}

	getResult := bot.Dispatch(ctx, ownerRequest("config", "get", zulipbot.KeyRestartStartupNotification))
	if !strings.Contains(getResult.Content, "true") {
		t.Fatalf("get reply = %q", getResult.Content)
	}

	listResult := bot.Dispatch(ctx, ownerRequest("config", "list"))
	if !strings.Contains(listResult.Content, zulipbot.KeyRestartStartupNotification) {
		t.Fatalf("list reply = %q", listResult.Content)
	}
}

func TestDispatchConfigRejectsInvalidValue(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(
		context.Background(),
		ownerRequest("config", "set", zulipbot.KeyRestartStartupNotification, "maybe"),
	)
	if !strings.Contains(result.Content, "Invalid value") {
		t.Fatalf("invalid value reply = %q", result.Content)
	}
}

func TestDispatchConfigUnknownKey(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), ownerRequest("config", "get", "does_not_exist"))
	if !strings.Contains(result.Content, "Unknown configuration key") {
		t.Fatalf("unknown key reply = %q", result.Content)
	}
}

func TestDispatchConfigDeniesNonAdmin(t *testing.T) {
	t.Parallel()

	bot := newDispatchTestBot(t)
	result := bot.Dispatch(context.Background(), memberRequest("config", "list"))
	if !strings.Contains(result.Content, "permission denied") {
		t.Fatalf("non-admin reply = %q", result.Content)
	}
}
