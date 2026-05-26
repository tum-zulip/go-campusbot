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

type fakeRoleService struct {
	roles   map[int64]string
	setErr  error
	listErr error
}

func newFakeRoleService() *fakeRoleService {
	return &fakeRoleService{roles: make(map[int64]string)}
}

func (s *fakeRoleService) GetUserRole(ctx context.Context, userID int64) (string, bool, error) {
	role, ok := s.roles[userID]
	return role, ok, nil
}

func (s *fakeRoleService) SetUserRole(ctx context.Context, userID int64, role string, grantedByUserID int64) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.roles[userID] = role
	return nil
}

func (s *fakeRoleService) ListUserRoles(ctx context.Context) ([]RoleRecord, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	var records []RoleRecord
	for userID, role := range s.roles {
		records = append(records, RoleRecord{UserID: userID, Role: role, GrantedByUserID: 1})
	}
	return records, nil
}

// ownerAuthorizer always allows owner-level checks.
type ownerAuthorizer struct{}

func (ownerAuthorizer) Check(ctx context.Context, actor model.Actor, perm permissions.Permission) error {
	return nil
}

// noneAuthorizer always denies non-None checks.
type noneAuthorizer struct{}

func (noneAuthorizer) Check(ctx context.Context, actor model.Actor, perm permissions.Permission) error {
	if perm == permissions.PermissionNone {
		return nil
	}
	return permissions.ErrDenied
}

func adminRequest(name string, args ...string) command.Request {
	return command.Request{
		Invocation: command.Invocation{Name: name, Args: args},
		Actor:      model.Actor{UserID: 1},
	}
}

func TestRoleHandlerListEmpty(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "list"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "No user roles") {
		t.Errorf("expected empty list message, got: %q", result.Content)
	}
}

func TestRoleHandlerListWithRoles(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	svc.roles[42] = "admin"
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "list"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "user_id=42") {
		t.Errorf("expected user_id=42 in list, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "role=admin") {
		t.Errorf("expected role=admin in list, got: %q", result.Content)
	}
}

func TestRoleHandlerListDistinguishesZulipOwnerAndLocalAdmin(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	svc.roles[42] = "owner"
	svc.roles[10] = "admin"
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "list"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "user_id=42 role=owner") ||
		!strings.Contains(result.Content, "Zulip-derived") {
		t.Fatalf("expected Zulip-derived owner in list, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "user_id=10 role=admin") || !strings.Contains(result.Content, "local SQLite") {
		t.Fatalf("expected local SQLite admin in list, got: %q", result.Content)
	}
}

func TestRoleHandlerGetFound(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	svc.roles[42] = "owner"
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "get", "42"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "owner") {
		t.Errorf("expected 'owner' in response, got: %q", result.Content)
	}
}

func TestRoleHandlerGetDistinguishesZulipOwner(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	svc.roles[42] = "owner"
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "get", "42"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "role: owner") || !strings.Contains(result.Content, "Zulip-derived") {
		t.Fatalf("expected Zulip-derived owner response, got: %q", result.Content)
	}
}

func TestRoleHandlerGetNotFound(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "get", "99"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "no local role") {
		t.Errorf("expected 'no local role' message, got: %q", result.Content)
	}
	if !strings.Contains(result.Content, "role: none") {
		t.Errorf("expected explicit default none role, got: %q", result.Content)
	}
}

func TestRoleHandlerSet(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	result, err := handler.Handle(context.Background(), adminRequest("role", "set", "42", "admin"))
	if err != nil {
		t.Fatalf("Handle() failed: %v", err)
	}
	if !strings.Contains(result.Content, "admin") {
		t.Errorf("expected 'admin' in confirmation, got: %q", result.Content)
	}
	if svc.roles[42] != "admin" {
		t.Errorf("role not persisted, got: %q", svc.roles[42])
	}
}

func TestRoleHandlerSetRequiresOwner(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	// Use noneAuthorizer which denies PermissionOwner
	handler := NewRoleHandler(svc, noneAuthorizer{})

	_, err := handler.Handle(context.Background(), adminRequest("role", "set", "42", "admin"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError when not owner, got: %v", err)
	}
}

func TestRoleHandlerSetInvalidRole(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	svc.setErr = errors.New("invalid role")
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	_, err := handler.Handle(context.Background(), adminRequest("role", "set", "42", "superuser"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for invalid role, got: %v", err)
	}
}

func TestRoleHandlerGetInvalidUserID(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	_, err := handler.Handle(context.Background(), adminRequest("role", "get", "notanumber"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for invalid user ID, got: %v", err)
	}
}

func TestRoleHandlerNoArgs(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	_, err := handler.Handle(context.Background(), adminRequest("role"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError for no args, got: %v", err)
	}
}

func TestRoleHandlerMetadata(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})
	meta := handler.Metadata()
	if meta.Name != "role" {
		t.Errorf("name = %q, want 'role'", meta.Name)
	}
	if !meta.Privileged {
		t.Error("role command should be privileged")
	}
}

func TestRoleHandlerSetRejectsOwnerRole(t *testing.T) {
	t.Parallel()

	svc := newFakeRoleService()
	handler := NewRoleHandler(svc, ownerAuthorizer{})

	_, err := handler.Handle(context.Background(), adminRequest("role", "set", "42", "owner"))
	var userErr command.UserError
	if !errors.As(err, &userErr) {
		t.Fatalf("expected UserError when setting owner, got: %v", err)
	}
}
