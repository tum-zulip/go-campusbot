package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tum-zulip/go-zulip/zulip"
)

// staticRoleProvider implements RoleProvider with a fixed role (or error).
type staticRoleProvider struct {
	role zulip.Role
	err  error
}

func (p staticRoleProvider) RoleFor(_ context.Context, _ Actor) (zulip.Role, error) {
	return p.role, p.err
}

// buildHelpTestRegistry builds a registry with one command per permission level
// plus a "role" command that has OwnerUsage set (to mirror real bot behaviour).
func buildHelpTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry := NewRegistry()

	mustRegister := func(meta Metadata) {
		t.Helper()
		if err := registry.Register(HandlerFunc{
			Meta: meta,
			Fn: func(ctx context.Context, req Request) (Result, error) {
				return Result{Content: "ok"}, nil
			},
		}); err != nil {
			t.Fatalf("Register(%q) failed: %v", meta.Name, err)
		}
	}

	// Public — everyone.
	mustRegister(Metadata{
		Name:       "status",
		Summary:    "Show bot status.",
		Usage:      "status",
		Permission: PermOpen,
	})

	// Admin — admin or owner.
	mustRegister(Metadata{
		Name:       "config",
		Summary:    "Read or update bot configuration.",
		Usage:      "config <list|get|set>",
		Permission: PermAdmin,
	})

	// Admin with owner-only subcommand.
	mustRegister(Metadata{
		Name:       "role",
		Summary:    "Manage user roles.",
		Usage:      "role <list|get <user-id>>",
		OwnerUsage: "role <list|get <user-id>|set <user-id> <role>>",
		Permission: PermAdmin,
	})

	// Owner only.
	mustRegister(Metadata{
		Name:       "restart",
		Summary:    "Gracefully restart the bot process.",
		Usage:      "restart",
		Permission: PermOwner,
	})

	return registry
}

// runHelp calls HelpHandler.Handle with the given actor and optional args.
func runHelp(t *testing.T, h *HelpHandler, actor Actor, args ...string) (string, error) {
	t.Helper()
	result, err := h.Handle(context.Background(), Request{
		Invocation: Invocation{Name: "help", Args: args},
		Actor:      actor,
	})
	return result.Content, err
}

// --- List help tests ---

func TestHelpNoneUserSeesOnlyPublicCommands(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleMember})

	out, err := runHelp(t, h, Actor{UserID: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "status") {
		t.Errorf("member user should see 'status', got: %q", out)
	}
	if strings.Contains(out, "config") {
		t.Errorf("member user should NOT see 'config', got: %q", out)
	}
	if strings.Contains(out, "role") {
		t.Errorf("member user should NOT see 'role', got: %q", out)
	}
	if strings.Contains(out, "restart") {
		t.Errorf("member user should NOT see 'restart', got: %q", out)
	}
}

func TestHelpAdminSeesAdminCommandsNotOwnerOnly(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleAdmin})

	out, err := runHelp(t, h, Actor{UserID: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "status") {
		t.Errorf("admin should see 'status', got: %q", out)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("admin should see 'config', got: %q", out)
	}
	if !strings.Contains(out, "role") {
		t.Errorf("admin should see 'role', got: %q", out)
	}
	// restart is owner-only — must not appear for admin.
	if strings.Contains(out, "restart") {
		t.Errorf("admin should NOT see 'restart', got: %q", out)
	}
	// 'role set' subcommand is owner-only — admin sees role usage without it.
	if strings.Contains(out, "set <user-id>") {
		t.Errorf("admin should NOT see 'role set <user-id>' in usage text, got: %q", out)
	}
}

func TestHelpOwnerSeesAllCommandsAndOwnerUsage(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleOwner})

	out, err := runHelp(t, h, Actor{UserID: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "status") {
		t.Errorf("owner should see 'status', got: %q", out)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("owner should see 'config', got: %q", out)
	}
	if !strings.Contains(out, "role") {
		t.Errorf("owner should see 'role', got: %q", out)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("owner should see 'restart', got: %q", out)
	}
	// Owner should see the OwnerUsage variant that includes 'role set <user-id>'.
	if !strings.Contains(out, "set <user-id>") {
		t.Errorf("owner should see 'role set <user-id>' in usage text, got: %q", out)
	}
}

// --- Fail-closed: permission lookup failure ---

func TestHelpPermissionLookupFailureShowsOnlyPublicCommands(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{err: errors.New("db connection lost")})

	out, err := runHelp(t, h, Actor{UserID: 99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "status") {
		t.Errorf("fallback should show 'status', got: %q", out)
	}
	if strings.Contains(out, "config") {
		t.Errorf("fallback must NOT leak 'config', got: %q", out)
	}
	if strings.Contains(out, "role") {
		t.Errorf("fallback must NOT leak 'role', got: %q", out)
	}
	if strings.Contains(out, "restart") {
		t.Errorf("fallback must NOT leak 'restart', got: %q", out)
	}
}

func TestHelpNilRoleProviderShowsOnlyPublicCommands(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, nil)

	out, err := runHelp(t, h, Actor{UserID: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "status") {
		t.Errorf("nil provider should show 'status', got: %q", out)
	}
	if strings.Contains(out, "config") || strings.Contains(out, "restart") {
		t.Errorf("nil provider must NOT show privileged commands, got: %q", out)
	}
}

// --- help <specific-command>: restricted commands must look "unknown" ---

func TestHelpNoneUserUnknownCommandDoesNotLeakRestrictedName(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleMember})

	// "restart" exists but member user cannot see it — must get "Unknown command".
	_, err := runHelp(t, h, Actor{UserID: 1}, "restart")
	if err == nil {
		t.Fatal("expected error for restricted command lookup by member user")
	}
	var userErr UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Unknown command") {
		t.Errorf("expected 'Unknown command' message, got: %q", userErr.Message)
	}
	// The error must not contain description or usage details for restart.
	if strings.Contains(userErr.Message, "Gracefully") || strings.Contains(userErr.Message, "restart the bot") {
		t.Errorf("error must not reveal restart description, got: %q", userErr.Message)
	}
}

func TestHelpAdminUnknownCommandDoesNotLeakOwnerOnly(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleAdmin})

	// "restart" is owner-only — admin must get "Unknown command", not its details.
	_, err := runHelp(t, h, Actor{UserID: 2}, "restart")
	if err == nil {
		t.Fatal("expected error for owner-only command lookup by admin")
	}
	var userErr UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError, got %T: %v", err, err)
	}
	if !strings.Contains(userErr.Message, "Unknown command") {
		t.Errorf("expected 'Unknown command' message for admin, got: %q", userErr.Message)
	}
}

func TestHelpOwnerCanLookUpRestartDetails(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleOwner})

	out, err := runHelp(t, h, Actor{UserID: 3}, "restart")
	if err != nil {
		t.Fatalf("owner should be able to look up restart, got error: %v", err)
	}
	if !strings.Contains(out, "restart") {
		t.Errorf("owner restart detail should mention 'restart', got: %q", out)
	}
}

func TestHelpAdminCanLookUpRoleDetails(t *testing.T) {
	t.Parallel()

	registry := buildHelpTestRegistry(t)
	h := NewHelpHandler(registry, staticRoleProvider{role: zulip.RoleAdmin})

	out, err := runHelp(t, h, Actor{UserID: 2}, "role")
	if err != nil {
		t.Fatalf("admin should be able to look up role details, got error: %v", err)
	}
	// Admin sees role detail — but the usage must use the non-owner Usage (no set).
	if !strings.Contains(out, "role") {
		t.Errorf("role detail should contain 'role', got: %q", out)
	}
	if strings.Contains(out, "set") {
		t.Errorf("admin role detail must not mention 'set', got: %q", out)
	}
}
