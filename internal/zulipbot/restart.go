package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

type restartState struct {
	accepting atomic.Bool
	requested atomic.Bool
}

func newRestartState() *restartState {
	state := &restartState{}
	state.accepting.Store(true)
	return state
}

func (state *restartState) Accepting() bool {
	return state.accepting.Load()
}

func (state *restartState) RestartRequested() bool {
	return state.requested.Load()
}

func (state *restartState) requestRestart() bool {
	if !state.requested.CompareAndSwap(false, true) {
		return false
	}
	state.accepting.Store(false)
	return true
}

func (app *App) ScheduleRestart(
	ctx context.Context,
	actor command.Actor,
	messageID int64,
	target command.ReplyTarget,
) (int64, bool, error) {
	id, err := app.repo.CreateRestartRequest(ctx, storage.RestartRequest{
		RequestedByUserID: actor.UserID,
		RequestMessageID:  messageID,
		Target:            target,
	})
	if err != nil {
		return 0, false, err
	}
	scheduled := app.restart.requestRestart()
	return id, scheduled, nil
}

func (app *App) MarkRestartInProgress(ctx context.Context) error {
	id, ok, err := app.repo.LatestActiveRestartRequestID(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no active restart request")
	}
	return app.repo.MarkRestartInProgress(ctx, id)
}

func (app *App) NotifyRestartComplete(ctx context.Context) error {
	request, ok, err := app.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	messageID, sendErr := app.messenger.SendReply(
		ctx,
		request.Target,
		"Restart complete. Event processing is back online.",
	)
	if sendErr != nil {
		if completeErr := app.repo.CompleteRestartRequest(ctx, request.ID, 0, sendErr.Error()); completeErr != nil {
			app.logger.WarnContext(ctx, "failed to mark restart notification failure",
				"restart_request_id", request.ID, "error", completeErr)
		}
		return fmt.Errorf("send restart completion notification: %w", sendErr)
	}
	return app.repo.CompleteRestartRequest(ctx, request.ID, messageID, "")
}

func (app *App) MarkRestartComplete(ctx context.Context) error {
	request, ok, err := app.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return app.repo.CompleteRestartRequest(ctx, request.ID, 0, "")
}
