package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
)

type ConfigHandler struct {
	service *configsvc.Service
}

func NewConfigHandler(service *configsvc.Service) *ConfigHandler {
	return &ConfigHandler{service: service}
}

func (handler *ConfigHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:       "config",
		Summary:    "Read or update bot configuration.",
		Usage:      "config <list|get|set> [key] [value]",
		Permission: zulip.RoleAdmin,
		Privileged: true,
	}
}

func (handler *ConfigHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) == 0 {
		return command.Result{}, command.NewUserError("Usage: `config <list|get|set> [key] [value]`")
	}

	switch req.Invocation.Args[0] {
	case "list":
		return handler.list(ctx, req)
	case "get":
		return handler.get(ctx, req)
	case "set":
		return handler.set(ctx, req)
	default:
		return command.Result{}, command.NewUserError("Usage: `config <list|get|set> [key] [value]`")
	}
}

func (handler *ConfigHandler) list(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 1 {
		return command.Result{}, command.NewUserError("Usage: `config list`")
	}
	values, err := handler.service.List(ctx, req.Actor)
	if err != nil {
		return command.Result{}, handler.userFacingConfigError(err, "read")
	}
	if len(values) == 0 {
		return command.Result{Content: "No configuration values are visible to you."}, nil
	}

	var builder strings.Builder
	builder.WriteString("Configuration:\n")
	for _, value := range values {
		builder.WriteString("- `")
		builder.WriteString(value.Definition.Key)
		builder.WriteString("` = `")
		builder.WriteString(configsvc.Redact(value))
		builder.WriteString("`")
		if value.IsDefault {
			builder.WriteString(" (default)")
		}
		builder.WriteByte('\n')
	}
	return command.Result{Content: strings.TrimSpace(builder.String())}, nil
}

func (handler *ConfigHandler) get(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 2 {
		return command.Result{}, command.NewUserError("Usage: `config get <key>`")
	}
	value, err := handler.service.Get(ctx, req.Actor, req.Invocation.Args[1])
	if err != nil {
		return command.Result{}, handler.userFacingConfigError(err, "read")
	}
	return command.Result{
		Content: fmt.Sprintf("`%s` = `%s`", value.Definition.Key, configsvc.Redact(value)),
	}, nil
}

func (handler *ConfigHandler) set(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 3 {
		return command.Result{}, command.NewUserError("Usage: `config set <key> <value>`")
	}
	_, newValue, err := handler.service.Set(
		ctx,
		req.Actor,
		req.MessageID,
		req.Invocation.Args[1],
		req.Invocation.Args[2],
	)
	if err != nil {
		return command.Result{}, handler.userFacingConfigError(err, "change")
	}
	return command.Result{
		Content: fmt.Sprintf("Configuration updated: `%s` = `%s`", newValue.Definition.Key, configsvc.Redact(newValue)),
	}, nil
}

func (handler *ConfigHandler) userFacingConfigError(err error, action string) error {
	if errors.Is(err, configsvc.ErrUnknownKey) {
		return command.NewUserError("Unknown configuration key.")
	}
	if errors.Is(err, command.ErrDenied) {
		return command.NewUserError(fmt.Sprintf("You are not authorized to %s that configuration value.", action))
	}
	if errors.Is(err, command.ErrPermissionUnavailable) {
		return command.NewUserError("I cannot verify permissions right now, so I will not access configuration.")
	}
	return err
}
