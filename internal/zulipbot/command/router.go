package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tum-zulip/go-zulip/zulip"
)

type Authorizer interface {
	Check(ctx context.Context, actor Actor, minRole zulip.Role) error
}

type Router struct {
	registry  *Registry
	auth      Authorizer
	argParser *ArgParser
	accepting func() bool
	logger    *slog.Logger
}

type RouterConfig struct {
	Registry  *Registry
	Auth      Authorizer
	ArgParser *ArgParser
	Accepting func() bool
	Logger    *slog.Logger
}

func NewRouter(cfg RouterConfig) (*Router, error) {
	if cfg.Registry == nil {
		return nil, errors.New("command registry must not be nil")
	}
	if cfg.Auth == nil {
		return nil, errors.New("command authorizer must not be nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Router{
		registry:  cfg.Registry,
		auth:      cfg.Auth,
		argParser: cfg.ArgParser,
		accepting: cfg.Accepting,
		logger:    cfg.Logger,
	}, nil
}

func (router *Router) Route(ctx context.Context, req Request) Result {
	handler, ok := router.registry.Lookup(req.Invocation.Name)
	if !ok {
		return Result{
			Content: fmt.Sprintf("Unknown command %q. Use `help` to see supported commands.", req.Invocation.Name),
		}
	}

	meta := handler.Metadata()
	if router.accepting != nil && !router.accepting() {
		return Result{Content: "The bot is restarting and is not accepting new commands right now."}
	}

	if err := router.auth.Check(ctx, req.Actor, meta.Permission); err != nil {
		router.logger.WarnContext(
			ctx,
			"command permission denied",
			"command",
			meta.Name,
			"actor_user_id",
			req.Actor.UserID,
			"error",
			err,
		)
		if errors.Is(err, ErrPermissionUnavailable) {
			return Result{Content: "I cannot verify permissions right now, so I will not run that command."}
		}
		return Result{Content: "permission denied"}
	}

	if meta.ArgSpec != nil && router.argParser != nil {
		parsed, parseErr := router.argParser.Parse(ctx, meta.ArgSpec, req.Invocation.Args)
		if parseErr != nil {
			var userErr UserError
			if errors.As(parseErr, &userErr) {
				return Result{Content: userErr.Message}
			}
			router.logger.ErrorContext(ctx, "arg parsing failed", "command", meta.Name, "error", parseErr)
			return Result{Content: "Command failed because of an internal error."}
		}
		req.ParsedArgs = parsed
	}

	result, err := handler.Handle(ctx, req)
	if err == nil {
		return result
	}

	var userErr UserError
	if errors.As(err, &userErr) {
		return Result{Content: userErr.Message}
	}

	router.logger.ErrorContext(
		ctx,
		"command handler failed",
		"command",
		meta.Name,
		"actor_user_id",
		req.Actor.UserID,
		"error",
		err,
	)
	return Result{Content: "Command failed because of an internal error."}
}
