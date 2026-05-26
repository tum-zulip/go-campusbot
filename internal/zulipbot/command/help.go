package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
)

// RoleProvider resolves the Zulip organizational role for an actor.
// It is used by HelpHandler to filter visible commands by the actor's role.
// If role resolution fails, HelpHandler falls back to showing only public commands.
type RoleProvider interface {
	RoleFor(ctx context.Context, actor model.Actor) (zulip.Role, error)
}

type HelpHandler struct {
	registry *Registry
	roles    RoleProvider
}

// NewHelpHandler creates a HelpHandler.
// roles is used to determine which commands are visible to the requesting actor.
// If roles is nil, only public (PermOpen) commands are shown.
func NewHelpHandler(registry *Registry, roles RoleProvider) *HelpHandler {
	return &HelpHandler{registry: registry, roles: roles}
}

func (handler *HelpHandler) Metadata() Metadata {
	return Metadata{
		Name:       "help",
		Summary:    "Show commands available to you.",
		Usage:      "help [command]",
		Permission: PermOpen,
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

// actorRole resolves the actor's Zulip role, falling back to RoleMember on any error.
// This ensures that a lookup failure never leaks admin/owner commands in help output.
func (handler *HelpHandler) actorRole(ctx context.Context, actor model.Actor) zulip.Role {
	if handler.roles == nil {
		return zulip.RoleMember
	}
	role, err := handler.roles.RoleFor(ctx, actor)
	if err != nil {
		return zulip.RoleMember
	}
	return role
}

// visibleMetas returns the subset of registered commands the actor may run.
func (handler *HelpHandler) visibleMetas(role zulip.Role) []Metadata {
	all := handler.registry.Metadata()
	visible := make([]Metadata, 0, len(all))
	for _, meta := range all {
		if roleAllows(role, meta.Permission) {
			visible = append(visible, meta)
		}
	}
	return visible
}

// formatHelp renders the help text for the given command list and actor role.
func formatHelp(metas []Metadata, role zulip.Role) string {
	var builder strings.Builder
	builder.WriteString("Supported commands (send as a private message, no prefix needed):\n")
	for _, meta := range metas {
		usage := meta.Usage
		if meta.OwnerUsage != "" && role <= zulip.RoleOwner {
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

// roleAllows returns true if actorRole has at least the required privilege level.
// A required role of 0 (PermOpen) allows everyone.
// Lower numeric Zulip role values represent higher privilege (owner=100 < admin=200 < member=400).
func roleAllows(actorRole, requiredRole zulip.Role) bool {
	return requiredRole == 0 || actorRole <= requiredRole
}
