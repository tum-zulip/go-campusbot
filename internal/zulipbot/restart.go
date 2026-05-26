package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
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
	actor model.Actor,
	messageID int64,
	target model.ReplyTarget,
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

type replyMessenger interface {
	SendReply(ctx context.Context, target model.ReplyTarget, content string) (int64, error)
}

type startupNotifier struct {
	repo      *storage.Repository
	messenger replyMessenger
	logger    *slog.Logger
}

func newStartupNotifier(repo *storage.Repository, messenger replyMessenger, logger *slog.Logger) *startupNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &startupNotifier{repo: repo, messenger: messenger, logger: logger}
}

func (notifier *startupNotifier) NotifyRestartComplete(ctx context.Context) error {
	request, ok, err := notifier.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	messageID, sendErr := notifier.messenger.SendReply(
		ctx,
		request.Target,
		"Restart complete. Event processing is back online.",
	)
	if sendErr != nil {
		if completeErr := notifier.repo.CompleteRestartRequest(ctx, request.ID, 0, sendErr.Error()); completeErr != nil {
			notifier.logger.WarnContext(
				ctx,
				"failed to mark restart notification failure",
				"restart_request_id",
				request.ID,
				"error",
				completeErr,
			)
		}
		return fmt.Errorf("send restart completion notification: %w", sendErr)
	}
	if completeErr := notifier.repo.CompleteRestartRequest(ctx, request.ID, messageID, ""); completeErr != nil {
		return completeErr
	}
	return nil
}

func (notifier *startupNotifier) MarkRestartComplete(ctx context.Context) error {
	request, ok, err := notifier.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return notifier.repo.CompleteRestartRequest(ctx, request.ID, 0, "")
}
