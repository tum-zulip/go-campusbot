package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

func TestRouterRejectsUnauthorizedCommandAndAudits(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	err := registry.Register(command.HandlerFunc{
		Meta: command.Metadata{
			Name:       "restart",
			Usage:      "restart",
			Permission: command.PermOwner,
			Privileged: true,
		},
		Fn: func(_ context.Context, _ command.Request) (command.Result, error) {
			return command.Result{Content: "should not run"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	auditor := &recordingAuditor{}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     denyingAuthorizer{},
		Auditor:  auditor,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 123},
		MessageID:  456,
	})
	if result.Content != "permission denied" {
		t.Fatalf("Content = %q", result.Content)
	}
	if len(auditor.records) != 1 {
		t.Fatalf("audit record count = %d, want 1", len(auditor.records))
	}
	if auditor.records[0].Status != audit.StatusDenied {
		t.Fatalf("audit status = %q, want denied", auditor.records[0].Status)
	}
}

func TestRouterMapsUserErrorsToResponses(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	err := registry.Register(command.HandlerFunc{
		Meta: command.Metadata{Name: "config", Usage: "config", Permission: command.PermOpen},
		Fn: func(_ context.Context, _ command.Request) (command.Result, error) {
			return command.Result{}, command.NewUserError("Usage: `config list`")
		},
	})
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     allowingAuthorizer{},
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(context.Background(), command.Request{Invocation: command.Invocation{Name: "config"}})
	if result.Content != "Usage: `config list`" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestRouterEnforcesRealPermissionRolesForAdminCommand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// User 3 is the Zulip org owner; users 1, 2, 999 are regular members.
	auth := fakeAuthorizer{3: zulip.RoleOwner}

	var ran int
	registry := command.NewRegistry()
	if err := registry.Register(command.HandlerFunc{
		Meta: command.Metadata{
			Name:       "restart",
			Usage:      "restart",
			Permission: command.PermOwner,
			Privileged: true,
		},
		Fn: func(_ context.Context, _ command.Request) (command.Result, error) {
			ran++
			return command.Result{Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	for _, userID := range []int64{1, 2, 999} {
		result := router.Route(
			ctx,
			command.Request{Invocation: command.Invocation{Name: "restart"}, Actor: command.Actor{UserID: userID}},
		)
		if result.Content != "permission denied" {
			t.Fatalf("user %d content = %q", userID, result.Content)
		}
	}
	result := router.Route(
		ctx,
		command.Request{Invocation: command.Invocation{Name: "restart"}, Actor: command.Actor{UserID: 3}},
	)
	if result.Content != "ok" {
		t.Fatalf("owner content = %q, want ok", result.Content)
	}
	if ran != 1 {
		t.Fatalf("handler ran %d times, want 1", ran)
	}
}

// fakeAuthorizer maps user IDs to Zulip roles; unmapped users get RoleMember.
type fakeAuthorizer map[int64]zulip.Role

func (f fakeAuthorizer) Check(_ context.Context, actor command.Actor, minRole zulip.Role) error {
	if minRole == 0 {
		return nil
	}
	role, ok := f[actor.UserID]
	if !ok {
		role = zulip.RoleMember
	}
	if role <= minRole {
		return nil
	}
	return command.ErrDenied
}

type denyingAuthorizer struct{}

func (denyingAuthorizer) Check(_ context.Context, _ command.Actor, _ zulip.Role) error {
	return command.ErrDenied
}

type allowingAuthorizer struct{}

func (allowingAuthorizer) Check(_ context.Context, _ command.Actor, _ zulip.Role) error {
	return nil
}

type recordingAuditor struct {
	records []audit.Record
}

func (auditor *recordingAuditor) RecordAudit(_ context.Context, record audit.Record) error {
	if record.Action == "" {
		return errors.New("empty action")
	}
	auditor.records = append(auditor.records, record)
	return nil
}
