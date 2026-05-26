package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
)

// RoleRecord holds a single user-role assignment without importing storage.
type RoleRecord struct {
	UserID          int64
	Role            string
	GrantedByUserID int64
}

// RoleService provides role management operations for the RoleHandler.
type RoleService interface {
	GetUserRole(ctx context.Context, userID int64) (role string, found bool, err error)
	SetUserRole(ctx context.Context, userID int64, role string, grantedByUserID int64) error
	ListUserRoles(ctx context.Context) ([]RoleRecord, error)
}

// RoleAuthorizer checks whether an actor has a given permission.
type RoleAuthorizer interface {
	Check(ctx context.Context, actor model.Actor, permission permissions.Permission) error
}

// RoleHandler handles the 'role' command.
type RoleHandler struct {
	service    RoleService
	authorizer RoleAuthorizer
}

// NewRoleHandler creates a RoleHandler.
func NewRoleHandler(service RoleService, authorizer RoleAuthorizer) *RoleHandler {
	return &RoleHandler{service: service, authorizer: authorizer}
}

func (handler *RoleHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:    "role",
		Summary: "Manage user roles.",
		// Usage shown to admins: list and get only (set requires owner).
		Usage: "role <list|get <user-id>>",
		// OwnerUsage shown to owners: includes set subcommand.
		OwnerUsage: "role <list|get <user-id>|set <user-id> <role>>",
		Permission: permissions.PermissionAdmin,
		Privileged: true,
	}
}

func (handler *RoleHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) == 0 {
		return command.Result{}, command.NewUserError("Usage: `role <list|get <user-id>|set <user-id> <role>>`")
	}

	switch req.Invocation.Args[0] {
	case "list":
		return handler.list(ctx, req)
	case "get":
		return handler.get(ctx, req)
	case "set":
		return handler.set(ctx, req)
	default:
		return command.Result{}, command.NewUserError("Usage: `role <list|get <user-id>|set <user-id> <role>>`")
	}
}

func (handler *RoleHandler) list(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 1 {
		return command.Result{}, command.NewUserError("Usage: `role list`")
	}
	records, err := handler.service.ListUserRoles(ctx)
	if err != nil {
		return command.Result{}, err
	}
	if len(records) == 0 {
		return command.Result{Content: "No user roles assigned."}, nil
	}

	var sb strings.Builder
	sb.WriteString("User roles:\n")
	for _, r := range records {
		switch {
		case r.Role == string(permissions.RoleOwner):
			fmt.Fprintf(&sb, "- user_id=%d role=%s (Zulip-derived bot_owner_id; not stored locally)\n", r.UserID, r.Role)
		case r.GrantedByUserID != 0:
			fmt.Fprintf(&sb, "- user_id=%d role=%s (local SQLite, granted by user_id=%d)\n", r.UserID, r.Role, r.GrantedByUserID)
		default:
			fmt.Fprintf(&sb, "- user_id=%d role=%s (local SQLite)\n", r.UserID, r.Role)
		}
	}
	return command.Result{Content: strings.TrimSpace(sb.String())}, nil
}

func (handler *RoleHandler) get(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 2 {
		return command.Result{}, command.NewUserError("Usage: `role get <user-id>`")
	}
	userID, err := parseUserID(req.Invocation.Args[1])
	if err != nil {
		return command.Result{}, command.NewUserError("user-id must be a positive integer")
	}
	role, found, err := handler.service.GetUserRole(ctx, userID)
	if err != nil {
		return command.Result{}, err
	}
	if !found {
		return command.Result{Content: fmt.Sprintf("User %d has role: none (default; no local role assignment).", userID)}, nil
	}
	if role == string(permissions.RoleOwner) {
		return command.Result{Content: fmt.Sprintf("User %d has role: owner (Zulip-derived from bot_owner_id; not stored locally).", userID)}, nil
	}
	return command.Result{Content: fmt.Sprintf("User %d has role: %s (local SQLite role).", userID, role)}, nil
}

func (handler *RoleHandler) set(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 3 {
		return command.Result{}, command.NewUserError("Usage: `role set <user-id> <role>`")
	}

	// role set requires owner permission.
	if err := handler.authorizer.Check(ctx, req.Actor, permissions.PermissionOwner); err != nil {
		if errors.Is(err, permissions.ErrPermissionUnavailable) {
			return command.Result{}, command.NewUserError("I cannot verify permissions right now, so I will not set roles.")
		}
		return command.Result{}, command.NewUserError("You are not authorized to set roles.")
	}

	userID, err := parseUserID(req.Invocation.Args[1])
	if err != nil {
		return command.Result{}, command.NewUserError("user-id must be a positive integer")
	}
	roleStr := req.Invocation.Args[2]
	if roleStr == string(permissions.RoleOwner) {
		return command.Result{}, command.NewUserError(
			"Role 'owner' cannot be assigned through the bot. The bot owner is determined automatically from the Zulip API. Valid roles: none, admin.",
		)
	}
	if err := handler.service.SetUserRole(ctx, userID, roleStr, req.Actor.UserID); err != nil {
		return command.Result{}, command.NewUserError(fmt.Sprintf("Invalid role %q. Valid roles: none, admin.", roleStr))
	}
	return command.Result{Content: fmt.Sprintf("User %d role set to: %s", userID, roleStr)}, nil
}

func parseUserID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid user ID %q", s)
	}
	return id, nil
}
