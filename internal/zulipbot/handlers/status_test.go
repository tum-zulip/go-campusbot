package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

type fakeStatusProvider struct {
	uptimeSeconds  int64
	queueID        string
	lastEventID    int64
	queueOK        bool
	queueErr       error
	dbErr          error
	restartPending bool
	restartErr     error
	accepting      bool
}

func (p *fakeStatusProvider) UptimeSeconds() int64 { return p.uptimeSeconds }

func (p *fakeStatusProvider) QueueStatus(ctx context.Context) (string, int64, bool, error) {
	return p.queueID, p.lastEventID, p.queueOK, p.queueErr
}

func (p *fakeStatusProvider) DBReachable(ctx context.Context) error { return p.dbErr }

func (p *fakeStatusProvider) RestartPending(ctx context.Context) (bool, error) {
	return p.restartPending, p.restartErr
}

func (p *fakeStatusProvider) Accepting() bool { return p.accepting }

type fakeAdminChecker struct {
	allowUserID int64
}

func (c *fakeAdminChecker) Check(_ context.Context, actor command.Actor, _ zulip.Role) error {
	if actor.UserID == c.allowUserID {
		return nil
	}
	return command.ErrDenied
}

func TestStatusHandlerPublicOutputForNonAdmin(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{uptimeSeconds: 90, accepting: true}
	handler := NewStatusHandler(provider, &fakeAdminChecker{allowUserID: 99})

	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      command.Actor{UserID: 1},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "online") {
		t.Errorf("content should mention online, got: %q", result.Content)
	}
	if strings.Contains(result.Content, "queue_id") {
		t.Errorf("non-admin should not see queue_id, got: %q", result.Content)
	}
}

func TestStatusHandlerAdminSeesDetailedOutput(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{
		uptimeSeconds: 90,
		accepting:     true,
		queueID:       "test-queue",
		lastEventID:   42,
		queueOK:       true,
	}
	handler := NewStatusHandler(provider, &fakeAdminChecker{allowUserID: 10})

	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      command.Actor{UserID: 10},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "queue_id") {
		t.Errorf("admin should see queue_id, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "test-queue") {
		t.Errorf("admin should see queue ID value, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "db_reachable: yes") {
		t.Errorf("admin should see db_reachable, got: %q", result.Content)
	}
}
