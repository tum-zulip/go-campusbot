package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
)

// RoleProvider resolves the permission role for an actor.
// It is used by HelpHandler to filter visible commands by the actor's role.
// If role resolution fails, HelpHandler falls back to RoleNone (fail closed).
type RoleProvider interface {
	RoleFor(ctx context.Context, actor model.Actor) (permissions.Role, error)
}

type HelpHandler struct {
	registry *Registry
	roles    RoleProvider
}

// NewHelpHandler creates a HelpHandler.
// roles is used to determine which commands are visible to the requesting actor.
// If roles is nil, only public (PermissionNone) commands are shown.
func NewHelpHandler(registry *Registry, roles RoleProvider) *HelpHandler {
	return &HelpHandler{registry: registry, roles: roles}
}

func (handler *HelpHandler) Metadata() Metadata {
	return Metadata{
		Name:       "help",
		Summary:    "Show commands available to you.",
		Usage:      "help [command]",
		Permission: permissions.PermissionNone,
	}
}

func (handler *HelpHandler) Handle(ctx context.Context, req Request) (Result, error) {
	role := handler.actorRole(ctx, req.Actor)
	metas := handler.visibleMetas(role)

	if len(req.Invocation.Args) > 0 {
		name := strings.ToLower(req.Invocation.Args[0])
		for _, meta := range metas {
			if meta.Name == name {
				return Result{Content: formatHelp([]Metadata{meta}, role)}, nil
			}
		}
		// Return the same error regardless of whether the command exists but is
		// restricted — do not reveal restricted command names to the actor.
		return Result{}, NewUserError(fmt.Sprintf("Unknown command %q.", name))
	}

	return Result{Content: formatHelp(metas, role)}, nil
}

// actorRole resolves the actor's role, failing closed to RoleNone on any error.
// This ensures that a DB outage never leaks admin/owner commands in help output.
func (handler *HelpHandler) actorRole(ctx context.Context, actor model.Actor) permissions.Role {
	if handler.roles == nil {
		return permissions.RoleNone
	}
	role, err := handler.roles.RoleFor(ctx, actor)
	if err != nil {
		// Fail closed: if permission state is unavailable, show only public commands.
		return permissions.RoleNone
	}
	return role
}

// visibleMetas returns the subset of registered commands the actor may run.
func (handler *HelpHandler) visibleMetas(role permissions.Role) []Metadata {
	all := handler.registry.Metadata()
	visible := make([]Metadata, 0, len(all))
	for _, meta := range all {
		if role.Allows(meta.Permission) {
			visible = append(visible, meta)
		}
	}
	return visible
}

// formatHelp renders the help text for the given command list and actor role.
// When meta.OwnerUsage is set and the actor is an owner, the owner-specific
// usage string is shown (which may include owner-only subcommands).
func formatHelp(metas []Metadata, role permissions.Role) string {
	var builder strings.Builder
	builder.WriteString("Supported commands (send as a private message, no prefix needed):\n")
	for _, meta := range metas {
		usage := meta.Usage
		if meta.OwnerUsage != "" && role == permissions.RoleOwner {
			usage = meta.OwnerUsage
		}
		builder.WriteString("- `")
		builder.WriteString(usage)
		builder.WriteString("`")
		if meta.Summary != "" {
			builder.WriteString(" — ")
			builder.WriteString(meta.Summary)
		}
		builder.WriteByte('\n')
	}
	return strings.TrimSpace(builder.String())
}
