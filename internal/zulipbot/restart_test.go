package zulipbot //nolint:testpackage // exercises partial App wiring for restart state

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestScheduleRestartStopsAcceptingCommands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openRestartTestRepository(t)
	defer repo.Close()

	app := &App{repo: repo, restart: newRestartState()}

	if !app.restart.Accepting() {
		t.Fatal("app should be accepting before restart is scheduled")
	}

	_, _, err := app.ScheduleRestart(
		ctx,
		command.Actor{UserID: 1},
		10,
		command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{1}},
	)
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}

	if app.restart.Accepting() {
		t.Fatal("app should stop accepting after restart is scheduled")
	}
	if !app.restart.RestartRequested() {
		t.Fatal("restart should be requested")
	}
}

func TestDoubleScheduleRestartIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openRestartTestRepository(t)
	defer repo.Close()

	app := &App{repo: repo, restart: newRestartState()}
	target := command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{1}}

	_, first, err := app.ScheduleRestart(ctx, command.Actor{UserID: 1}, 1, target)
	if err != nil {
		t.Fatalf("first ScheduleRestart() failed: %v", err)
	}
	if !first {
		t.Fatal("first ScheduleRestart() should return scheduled=true")
	}

	_, second, err := app.ScheduleRestart(ctx, command.Actor{UserID: 1}, 2, target)
	if err != nil {
		t.Fatalf("second ScheduleRestart() failed: %v", err)
	}
	if second {
		t.Fatal("second ScheduleRestart() should return scheduled=false")
	}
}

func TestNotifyRestartCompleteCompletesPendingRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openRestartTestRepository(t)
	defer repo.Close()

	target := command.ReplyTarget{Kind: command.ReplyKindDirect, UserIDs: []int64{10}}
	id, err := repo.CreateRestartRequest(
		ctx,
		storage.RestartRequest{RequestedByUserID: 10, RequestMessageID: 55, Target: target},
	)
	if err != nil {
		t.Fatalf("CreateRestartRequest() failed: %v", err)
	}

	fakeMsg := &fakeRestartMessenger{}
	app := &App{repo: repo, restart: newRestartState(), messenger: fakeMsg, logger: slog.Default()}
	if notifyErr := app.NotifyRestartComplete(ctx); notifyErr != nil {
		t.Fatalf("NotifyRestartComplete() failed: %v", notifyErr)
	}
	if fakeMsg.sentTo.UserIDs[0] != 10 {
		t.Fatalf("sent target = %#v", fakeMsg.sentTo)
	}
	_, ok, err := repo.PendingRestartRequest(ctx)
	if err != nil {
		t.Fatalf("PendingRestartRequest() failed: %v", err)
	}
	if ok {
		t.Fatalf("restart request %d should be completed", id)
	}
}

type fakeRestartMessenger struct {
	sentTo command.ReplyTarget
}

func (messenger *fakeRestartMessenger) SendReply(
	_ context.Context,
	target command.ReplyTarget,
	_ string,
) (int64, error) {
	messenger.sentTo = target
	return 123, nil
}

func openRestartTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
