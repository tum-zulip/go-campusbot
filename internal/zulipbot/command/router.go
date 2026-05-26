package command

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/audit"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
)

type Authorizer interface {
	Check(ctx context.Context, actor model.Actor, minRole zulip.Role) error
}

type Auditor interface {
	RecordAudit(ctx context.Context, record audit.Record) error
}

type Router struct {
	registry  *Registry
	auth      Authorizer
	auditor   Auditor
	accepting func() bool
	logger    *slog.Logger
}

type RouterConfig struct {
	Registry  *Registry
	Auth      Authorizer
	Auditor   Auditor
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
		auditor:   cfg.Auditor,
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
		router.audit(ctx, req, meta, audit.StatusDenied, "")
		return Result{Content: "The bot is restarting and is not accepting new commands right now."}
	}

	if err := router.auth.Check(ctx, req.Actor, meta.Permission); err != nil {
		router.audit(ctx, req, meta, audit.StatusDenied, "")
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

	result, err := handler.Handle(ctx, req)
	if err == nil {
		router.audit(ctx, req, meta, audit.StatusSuccess, "")
		return result
	}

	var userErr UserError
	if errors.As(err, &userErr) {
		router.audit(ctx, req, meta, audit.StatusFailure, userErr.Message)
		return Result{Content: userErr.Message}
	}

	router.audit(ctx, req, meta, audit.StatusFailure, "")
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

func (router *Router) audit(ctx context.Context, req Request, meta Metadata, status audit.Status, message string) {
	if router.auditor == nil || !meta.Privileged {
		return
	}
	record := audit.Record{
		ActorUserID: req.Actor.UserID,
		Action:      "command." + meta.Name,
		Target:      meta.Name,
		Status:      status,
		MessageID:   req.MessageID,
		Error:       message,
	}
	if err := router.auditor.RecordAudit(ctx, record); err != nil {
		router.logger.WarnContext(ctx, "failed to record command audit event", "command", meta.Name, "error", err)
	}
}
