package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
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

func (c *fakeAdminChecker) Check(ctx context.Context, actor model.Actor, perm permissions.Permission) error {
	if actor.UserID == c.allowUserID {
		return nil
	}
	return permissions.ErrDenied
}

func TestStatusHandlerBasicUserResponse(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{
		uptimeSeconds: 3665, // 1h 1m 5s
		accepting:     true,
	}
	adminChecker := &fakeAdminChecker{allowUserID: 999} // regular user won't be admin

	handler := NewStatusHandler(provider, adminChecker)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      model.Actor{UserID: 100}, // not admin
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}

	if !strings.Contains(result.Content, "online") {
		t.Errorf("response should contain 'online', got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "1h 1m 5s") {
		t.Errorf("response should contain uptime '1h 1m 5s', got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "accepting commands: yes") {
		t.Errorf("response should contain 'accepting commands: yes', got: %q", result.Content)
	}
	// Should NOT contain admin-only fields
	if strings.Contains(result.Content, "queue_id") {
		t.Errorf("non-admin response should not contain queue_id, got: %q", result.Content)
	}
}

func TestStatusHandlerAdminUserGetsExtendedInfo(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{
		uptimeSeconds:  120,
		accepting:      true,
		queueID:        "abc-123",
		lastEventID:    42,
		queueOK:        true,
		restartPending: false,
	}
	adminChecker := &fakeAdminChecker{allowUserID: 999}

	handler := NewStatusHandler(provider, adminChecker)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      model.Actor{UserID: 999}, // admin
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}

	if !strings.Contains(result.Content, "queue_id: abc-123") {
		t.Errorf("admin response should contain queue_id, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "last_event_id: 42") {
		t.Errorf("admin response should contain last_event_id, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "db_reachable: yes") {
		t.Errorf("admin response should contain db_reachable, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "restart_pending: false") {
		t.Errorf("admin response should contain restart_pending, got: %q", result.Content)
	}
}

func TestStatusHandlerNotAccepting(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{
		uptimeSeconds: 60,
		accepting:     false,
	}
	adminChecker := &fakeAdminChecker{allowUserID: -1}

	handler := NewStatusHandler(provider, adminChecker)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      model.Actor{UserID: 100},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "accepting commands: no") {
		t.Errorf("response should contain 'accepting commands: no', got: %q", result.Content)
	}
}

func TestStatusHandlerRejectsExtraArgs(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{accepting: true}
	adminChecker := &fakeAdminChecker{}

	handler := NewStatusHandler(provider, adminChecker)
	_, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status", Args: []string{"extra"}},
		Actor:      model.Actor{UserID: 100},
	})
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for extra args, got: %v", err)
	}
}

func TestStatusHandlerAdminDBUnreachable(t *testing.T) {
	t.Parallel()

	provider := &fakeStatusProvider{
		uptimeSeconds: 10,
		accepting:     true,
		dbErr:         errors.New("connection refused"),
	}
	adminChecker := &fakeAdminChecker{allowUserID: 999}

	handler := NewStatusHandler(provider, adminChecker)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "status"},
		Actor:      model.Actor{UserID: 999},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "db_reachable: no") {
		t.Errorf("response should indicate db unreachable, got: %q", result.Content)
	}
}
