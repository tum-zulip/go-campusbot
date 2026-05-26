package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
)

const permissionDenied = "permission denied"

func TestRestartHandlerSchedulesOnlyAfterAcknowledgementHook(t *testing.T) {
	t.Parallel()

	service := &fakeRestartService{}
	handler := handlers.NewRestartHandler(service)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 10},
		MessageID:  55,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{10}},
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
	_ command.Actor,
	_ int64,
	_ command.ReplyTarget,
) (int64, bool, error) {
	service.calls++
	return 1, true, nil
}

// TestRestartHandlerAcknowledgementContentIsPresent verifies the handler returns
// a non-empty confirmation message before the AfterResponse hook runs.
func TestRestartHandlerAcknowledgementContentIsPresent(t *testing.T) {
	t.Parallel()

	service := &fakeRestartService{}
	handler := handlers.NewRestartHandler(service)
	result, err := handler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 10},
		MessageID:  55,
		Target:     command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{10}},
	})
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if result.Content == "" {
		t.Fatal("restart should return a non-empty acknowledgement message")
	}
	if service.calls != 0 {
		t.Fatal("restart must not be scheduled before the response is sent")
	}
}

// TestRestartMetadataIsOwnerOnly verifies the metadata declares owner permission.
func TestRestartMetadataIsOwnerOnly(t *testing.T) {
	t.Parallel()

	handler := handlers.NewRestartHandler(&fakeRestartService{})
	meta := handler.Metadata()
	if meta.Permission != zulip.RoleOwner {
		t.Errorf("restart permission = %v, want %v (zulip.RoleOwner)", meta.Permission, zulip.RoleOwner)
	}
	if !meta.Privileged {
		t.Error("restart should be marked Privileged")
	}
}

// TestRestartRouterOwnerCanRun verifies that a Zulip org owner can run restart.
func TestRestartRouterOwnerCanRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := command.NewRegistry()
	if err := registry.Register(handlers.NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	// User 3 is the Zulip org owner.
	auth := fakeRestartAuth{3: zulip.RoleOwner}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 3},
	})
	if result.Content == permissionDenied {
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
	registry := command.NewRegistry()
	if err := registry.Register(handlers.NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	// User 2 is a Zulip admin; user 99 is the owner.
	auth := fakeRestartAuth{2: zulip.RoleAdmin, 99: zulip.RoleOwner}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 2},
	})
	if result.Content != permissionDenied {
		t.Fatalf("admin should be denied restart, got: %q", result.Content)
	}
}

// TestRestartRouterNoneUserCannotRun verifies a regular member cannot run restart.
func TestRestartRouterNoneUserCannotRun(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := command.NewRegistry()
	if err := registry.Register(handlers.NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	auth := fakeRestartAuth{}
	router, err := command.NewRouter(command.RouterConfig{
		Registry: registry,
		Auth:     auth,
	})
	if err != nil {
		t.Fatalf("NewRouter() failed: %v", err)
	}

	result := router.Route(ctx, command.Request{
		Invocation: command.Invocation{Name: "restart"},
		Actor:      command.Actor{UserID: 1},
	})
	if result.Content != permissionDenied {
		t.Fatalf("member should be denied restart, got: %q", result.Content)
	}
}

// TestRestartNotVisibleInHelpForAdmin verifies restart does not appear in
// help output for an admin actor.
func TestRestartNotVisibleInHelpForAdmin(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	if err := registry.Register(handlers.NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}
	if err := registry.Register(command.HandlerFunc{
		Meta: command.Metadata{
			Name:       "status",
			Summary:    "Status.",
			Usage:      "status",
			Permission: command.PermOpen,
		},
		Fn: func(_ context.Context, _ command.Request) (command.Result, error) {
			return command.Result{}, nil
		},
	}); err != nil {
		t.Fatalf("Register(status) failed: %v", err)
	}

	helpHandler := command.NewHelpHandler(registry, fixedRoleProvider{zulip.RoleAdmin})
	result, err := helpHandler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "help"},
		Actor:      command.Actor{UserID: 2},
	})
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if strings.Contains(result.Content, "restart") {
		t.Errorf("restart must not appear in admin help, got: %q", result.Content)
	}
}

// TestRestartNotVisibleInHelpForNoneUser verifies restart does not appear in
// help output for a regular member.
func TestRestartNotVisibleInHelpForNoneUser(t *testing.T) {
	t.Parallel()

	registry := command.NewRegistry()
	if err := registry.Register(handlers.NewRestartHandler(&fakeRestartService{})); err != nil {
		t.Fatalf("Register() failed: %v", err)
	}

	helpHandler := command.NewHelpHandler(registry, fixedRoleProvider{zulip.RoleMember})
	result, err := helpHandler.Handle(context.Background(), command.Request{
		Invocation: command.Invocation{Name: "help"},
		Actor:      command.Actor{UserID: 1},
	})
	if err != nil {
		t.Fatalf("help failed: %v", err)
	}
	if strings.Contains(result.Content, "restart") {
		t.Errorf("restart must not appear in member help, got: %q", result.Content)
	}
}

// fakeRestartAuth maps user IDs to Zulip roles; unmapped users get RoleMember.
type fakeRestartAuth map[int64]zulip.Role

func (f fakeRestartAuth) Check(_ context.Context, actor command.Actor, minRole zulip.Role) error {
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

// fixedRoleProvider implements command.RoleProvider with a fixed role.
type fixedRoleProvider struct {
	role zulip.Role
}

func (p fixedRoleProvider) RoleFor(_ context.Context, _ command.Actor) (zulip.Role, error) {
	return p.role, nil
}
