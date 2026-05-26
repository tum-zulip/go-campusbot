package handlers

import (
	"context"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

type RestartService interface {
	ScheduleRestart(
		ctx context.Context,
		actor command.Actor,
		messageID int64,
		target command.ReplyTarget,
	) (int64, bool, error)
}

type RestartHandler struct {
	service RestartService
}

func NewRestartHandler(service RestartService) *RestartHandler {
	return &RestartHandler{service: service}
}

func (handler *RestartHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:       "restart",
		Summary:    "Gracefully restart the bot process.",
		Usage:      "restart",
		Permission: zulip.RoleOwner,
		Privileged: true,
	}
}

func (handler *RestartHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 0 {
		return command.Result{}, command.NewUserError("Usage: `restart`")
	}
	return command.Result{
		Content: "Restarting now. I will resume the current Zulip event queue after the process comes back; Zulip normally retains queued events for about 10 minutes.",
		AfterResponse: func(ctx context.Context) error {
			_, _, err := handler.service.ScheduleRestart(ctx, req.Actor, req.MessageID, req.Target)
			return err
		},
	}, nil
}
