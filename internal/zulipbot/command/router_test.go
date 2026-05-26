package command

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestRouterRejectsUnauthorizedCommandAndAudits(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	err := registry.Register(HandlerFunc{
		Meta: Metadata{
			Name:       "restart",
			Usage:      "restart",
			Permission: permissions.PermissionOwner,
			Privileged: true,
		},
		Fn: func(ctx context.Context, req Request) (Result, error) {
			return Result{Content: "should not run"}, nil
		},
	})
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	auditor := &recordingAuditor{}
	router, err := NewRouter(RouterConfig{
		Registry: registry,
		Auth:     denyingAuthorizer{},
		Auditor:  auditor,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(context.Background(), Request{
		Invocation: Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 123},
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

	registry := NewRegistry()
	err := registry.Register(HandlerFunc{
		Meta: Metadata{Name: "config", Usage: "config", Permission: permissions.PermissionNone},
		Fn: func(ctx context.Context, req Request) (Result, error) {
			return Result{}, NewUserError("Usage: `config list`")
		},
	})
	if err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	router, err := NewRouter(RouterConfig{
		Registry: registry,
		Auth:     allowingAuthorizer{},
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(context.Background(), Request{Invocation: Invocation{Name: "config"}})
	if result.Content != "Usage: `config list`" {
		t.Fatalf("Content = %q", result.Content)
	}
}

func TestRouterEnforcesRealPermissionRolesForAdminCommand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo, err := storage.Open(ctx, filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer repo.Close()
	if err := repo.SetUserRole(ctx, 1, permissions.RoleNone, 0); err != nil {
		t.Fatalf("SetUserRole(none) failed: %v", err)
	}
	if err := repo.SetUserRole(ctx, 2, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole(admin) failed: %v", err)
	}
	// User 3 is the bot owner (resolved via ownerProvider, not stored in DB).

	var ran int
	registry := NewRegistry()
	if err := registry.Register(HandlerFunc{
		Meta: Metadata{
			Name:       "restart",
			Usage:      "restart",
			Permission: permissions.PermissionOwner,
			Privileged: true,
		},
		Fn: func(ctx context.Context, req Request) (Result, error) {
			ran++
			return Result{Content: "ok"}, nil
		},
	}); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	// Use ownerProvider to make user 3 the bot owner.
	router, err := NewRouter(RouterConfig{
		Registry: registry,
		Auth:     permissions.NewService(repo, staticOwnerID(3)),
		Auditor:  repo,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	for _, userID := range []int64{1, 2, 999} {
		result := router.Route(
			ctx,
			Request{Invocation: Invocation{Name: "restart"}, Actor: model.Actor{UserID: userID}},
		)
		if result.Content != "permission denied" {
			t.Fatalf("user %d content = %q", userID, result.Content)
		}
	}
	result := router.Route(ctx, Request{Invocation: Invocation{Name: "restart"}, Actor: model.Actor{UserID: 3}})
	if result.Content != "ok" {
		t.Fatalf("owner content = %q, want ok", result.Content)
	}
	if ran != 1 {
		t.Fatalf("handler ran %d times, want 1", ran)
	}
}

// staticOwnerID is a test OwnerProvider that returns a fixed user ID.
type staticOwnerID int64

func (id staticOwnerID) OwnerUserID() int64 { return int64(id) }

type denyingAuthorizer struct{}

func (denyingAuthorizer) Check(ctx context.Context, actor model.Actor, permission permissions.Permission) error {
	return permissions.ErrDenied
}

type allowingAuthorizer struct{}

func (allowingAuthorizer) Check(ctx context.Context, actor model.Actor, permission permissions.Permission) error {
	return nil
}

type recordingAuditor struct {
	records []audit.Record
}

func (auditor *recordingAuditor) RecordAudit(ctx context.Context, record audit.Record) error {
	if record.Action == "" {
		return errors.New("empty action")
	}
	auditor.records = append(auditor.records, record)
	return nil
}
