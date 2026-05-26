package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestRestartHandlerSchedulesOnlyAfterAcknowledgementHook(t *testing.T) {
	t.Parallel()

	service := &fakeRestartService{}
	handler := NewRestartHandler(service)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 10},
		MessageID:  55,
		Target:     model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10}},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if service.calls != 0 {
		t.Fatalf("restart scheduled before response hook ran")
	}
	if result.AfterResponse == nil {
		t.Fatal("restart result should include an AfterResponse hook")
	}
	if err := result.AfterResponse(context.Background()); err != nil {
		t.Fatalf("AfterResponse() failed: %v", err)
	}
	if service.calls != 1 {
		t.Fatalf("restart schedule calls = %d, want 1", service.calls)
	}
}

type fakeRestartService struct {
	calls int
}

func (service *fakeRestartService) ScheduleRestart(
	_ context.Context,
	_ model.Actor,
	_ int64,
	_ model.ReplyTarget,
) (int64, bool, error) {
	service.calls++
	return 1, true, nil
}

// TestRestartHandlerAcknowledgementContentIsPresent verifies the handler returns
// a non-empty confirmation message before the AfterResponse hook runs.
func TestRestartHandlerAcknowledgementContentIsPresent(t *testing.T) {
	t.Parallel()

	service := &fakeRestartService{}
	handler := NewRestartHandler(service)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 10},
		MessageID:  55,
		Target:     model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10}},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Fatal("restart should return a non-empty acknowledgement message")
	}
	// Ack must be sent before scheduling (service.calls==0 here).
	if service.calls != 0 {
		t.Fatal("restart must not be scheduled before the response is sent")
	}
}

// TestRestartMetadataIsOwnerOnly verifies the metadata declares owner permission.
func TestRestartMetadataIsOwnerOnly(t *testing.T) {
	t.Parallel()

	handler := NewRestartHandler(&fakeRestartService{})
	meta := handler.Metadata()
	if meta.Permission != permissions.PermissionOwner {
		t.Errorf("restart permission = %q, want %q", meta.Permission, permissions.PermissionOwner)
	}
	if !meta.Privileged {
		t.Error("restart should be marked Privileged")
	}
}

// TestRestartRouterOwnerCanRun verifies that an owner-authenticated actor
// can run restart end-to-end through the router.
func TestRestartRouterOwnerCanRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	repo, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer repo.Close()

	registry := command.NewRegistry()
	if err := registry.Register(NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	// Owner is user 3; no DB roles needed.
	type staticOwner int64
	auth := permissions.NewService(repo, staticOwnerIDForRestart(3))
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 3},
	})
	if result.Content == "permission denied" {
		t.Fatal("owner should be able to run restart")
	}
	if strings.Contains(result.Content, "not authorized") {
		t.Fatalf("unexpected denial for owner: %q", result.Content)
	}
}

// TestRestartRouterAdminCannotRun verifies admin cannot run the restart command.
func TestRestartRouterAdminCannotRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	repo, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer repo.Close()

	if err := repo.SetUserRole(ctx, 2, permissions.RoleAdmin, 0); err != nil {
		t.Fatalf("SetUserRole() failed: %v", err)
	}

	registry := command.NewRegistry()
	if err := registry.Register(NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	auth := permissions.NewService(repo, staticOwnerIDForRestart(99))
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 2},
	})
	if result.Content != "permission denied" {
		t.Fatalf("admin should be denied restart, got: %q", result.Content)
	}
}

// TestRestartRouterNoneUserCannotRun verifies a none user cannot run restart.
func TestRestartRouterNoneUserCannotRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bot.sqlite3")
	repo, err := storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	defer repo.Close()

	registry := command.NewRegistry()
	if err := registry.Register(NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	auth := permissions.NewService(repo, staticOwnerIDForRestart(99))
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      model.Actor{UserID: 1},
	})
	if result.Content != "permission denied" {
		t.Fatalf("none user should be denied restart, got: %q", result.Content)
	}
}

// TestRestartNotVisibleInHelpForAdmin verifies restart does not appear in
// help output for an admin actor.
func TestRestartNotVisibleInHelpForAdmin(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	if err := registry.Register(NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	if err := registry.Register(command.HandlerFunc{
		Meta: command.Metadata{
			Name:       "status",
			Summary:    "Status.",
			Usage:      "status",
			Permission: permissions.PermissionNone,
		},
		Fn: func(_ context.Context, _ command.Request) (command.Result, error) {
			return command.Result{}, nil
		},
	}); err != nil {
		t.Fatalf("Register(status) failed: %v", err)
	}

	helpHandler := command.NewHelpHandler(registry, fixedRoleProvider{permissions.RoleAdmin})
	result, err := helpHandler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "help"},
		Actor:      model.Actor{UserID: 2},
	})
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if strings.Contains(result.Content, "restart") {
		t.Errorf("restart must not appear in admin help, got: %q", result.Content)
	}
}

// TestRestartNotVisibleInHelpForNoneUser verifies restart does not appear in
// help output for a none user.
func TestRestartNotVisibleInHelpForNoneUser(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	if err := registry.Register(NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	helpHandler := command.NewHelpHandler(registry, fixedRoleProvider{permissions.RoleNone})
	result, err := helpHandler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "help"},
		Actor:      model.Actor{UserID: 1},
	})
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if strings.Contains(result.Content, "restart") {
		t.Errorf("restart must not appear in none-user help, got: %q", result.Content)
	}
}

// staticOwnerIDForRestart is a test OwnerProvider for restart tests.
type staticOwnerIDForRestart int64

func (id staticOwnerIDForRestart) OwnerUserID() int64 { return int64(id) }

// fixedRoleProvider implements command.RoleProvider with a fixed role.
type fixedRoleProvider struct {
	role permissions.Role
}

func (p fixedRoleProvider) RoleFor(_ context.Context, _ model.Actor) (permissions.Role, error) {
	return p.role, nil
}
