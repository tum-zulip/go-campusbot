package lifecycle

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

func TestServiceRequestsRestartAndExecutesInjectedExec(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	var execCalled bool
	manager, err := NewManager(ManagerConfig{
		Executable: "/bin/campusbot",
		Argv:       []string{"campusbot", "-db", "bot.sqlite3"},
		Env:        []string{"A=B"},
		Exec: func(path string, argv []string, env []string) error {
			execCalled = true
			if path != "/bin/campusbot" || argv[0] != "campusbot" || env[0] != "A=B" {
				t.Fatalf("unexpected exec args: %q %#v %#v", path, argv, env)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	service := NewService(repo, manager)

	_, scheduled, err := service.ScheduleRestart(ctx, model.Actor{UserID: 10}, 99, model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10}})
	if err != nil {
		t.Fatalf("RequestRestart() failed: %v", err)
	}
	if !scheduled {
		t.Fatal("first restart request should be scheduled")
	}
	if service.Accepting() {
		t.Fatal("service should stop accepting commands after restart request")
	}
	if err := service.MarkRestartInProgress(ctx); err != nil {
		t.Fatalf("MarkRestartInProgress() failed: %v", err)
	}
	if err := service.ExecRestart(); err != nil {
		t.Fatalf("ExecRestart() failed: %v", err)
	}
	if !execCalled {
		t.Fatal("injected exec was not called")
	}
}

func TestStartupNotifierCompletesPendingRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	target := model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10}}
	id, err := repo.CreateRestartRequest(ctx, storage.RestartRequest{RequestedByUserID: 10, RequestMessageID: 55, Target: target})
	if err != nil {
		t.Fatalf("CreateRestartRequest() failed: %v", err)
	}

	messenger := &fakeMessenger{}
	notifier := NewStartupNotifier(repo, messenger, nil)
	if err := notifier.NotifyRestartComplete(ctx); err != nil {
		t.Fatalf("NotifyRestartComplete() failed: %v", err)
	}
	if messenger.sentTo.UserIDs[0] != 10 {
		t.Fatalf("sent target = %#v", messenger.sentTo)
	}
	_, ok, err := repo.PendingRestartRequest(ctx)
	if err != nil {
		t.Fatalf("PendingRestartRequest() failed: %v", err)
	}
	if ok {
		t.Fatalf("restart request %d should be completed", id)
	}
}

type fakeMessenger struct {
	sentTo model.ReplyTarget
}

func (messenger *fakeMessenger) SendReply(ctx context.Context, target model.ReplyTarget, content string) (int64, error) {
	messenger.sentTo = target
	return 123, nil
}

// TestDryRunExecCompletesWithoutError verifies that a no-op ExecFunc
// (as used by -dry-run-restart) allows the restart flow to complete without error.
func TestDryRunExecCompletesWithoutError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	var execPath string
	dryRunExec := func(path string, argv []string, env []string) error {
		execPath = path
		return nil
	}

	manager, err := NewManager(ManagerConfig{
		Executable: "/bin/campusbot",
		Argv:       []string{"campusbot", "-db", "bot.sqlite3"},
		Env:        []string{"A=B"},
		Exec:       dryRunExec,
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	service := NewService(repo, manager)

	_, scheduled, err := service.ScheduleRestart(ctx, model.Actor{UserID: 10}, 99, model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{10}})
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}
	if !scheduled {
		t.Fatal("first restart request should be scheduled")
	}

	if err := service.MarkRestartInProgress(ctx); err != nil {
		t.Fatalf("MarkRestartInProgress() failed: %v", err)
	}

	if err := service.ExecRestart(); err != nil {
		t.Fatalf("dry-run ExecRestart() returned error: %v", err)
	}

	if execPath != "/bin/campusbot" {
		t.Fatalf("dry-run exec not called with expected path; got %q", execPath)
	}
}

// TestExecRestartReturnsErrorOnExecFailure verifies that ExecRestart surfaces
// errors returned by the injected exec function (e.g. permission errors, missing binary).
func TestExecRestartReturnsErrorOnExecFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	execErr := errors.New("exec: no such file or directory")
	manager, err := NewManager(ManagerConfig{
		Executable: "/nonexistent/campusbot",
		Argv:       []string{"campusbot"},
		Env:        []string{},
		Exec: func(path string, argv []string, env []string) error {
			return execErr
		},
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	service := NewService(repo, manager)

	_, _, err = service.ScheduleRestart(ctx, model.Actor{UserID: 1}, 1, model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{1}})
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}

	if err := service.ExecRestart(); !errors.Is(err, execErr) {
		t.Fatalf("ExecRestart() error = %v, want %v", err, execErr)
	}
}

// TestExecRestartRequiresRequestFirst verifies that ExecRestart fails if
// RequestRestart was never called (guards against accidental exec).
func TestExecRestartRequiresRequestFirst(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(ManagerConfig{
		Executable: "/bin/campusbot",
		Argv:       []string{"campusbot"},
		Env:        []string{},
		Exec: func(path string, argv []string, env []string) error {
			t.Error("exec must not be called without restart request")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	if err := manager.ExecRestart(); err == nil {
		t.Fatal("ExecRestart() should return error when restart not requested")
	}
}

// TestRequestRestartStopsAcceptingCommands verifies that once a restart is
// scheduled the manager stops reporting Accepting() == true.
func TestRequestRestartStopsAcceptingCommands(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	manager, err := NewManager(ManagerConfig{
		Executable: "/bin/campusbot",
		Argv:       []string{"campusbot"},
		Env:        []string{},
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	service := NewService(repo, manager)

	if !service.Accepting() {
		t.Fatal("service should be accepting before restart is scheduled")
	}

	_, _, err = service.ScheduleRestart(ctx, model.Actor{UserID: 1}, 10, model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{1}})
	if err != nil {
		t.Fatalf("ScheduleRestart() failed: %v", err)
	}

	if service.Accepting() {
		t.Fatal("service should stop accepting after restart is scheduled")
	}
}

// TestDoubleScheduleIsIdempotent verifies that a second ScheduleRestart
// does not overwrite the first request's DB row and returns scheduled=false.
func TestDoubleScheduleIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := openLifecycleTestRepository(t)
	defer repo.Close()

	manager, err := NewManager(ManagerConfig{
		Executable: "/bin/campusbot",
		Argv:       []string{"campusbot"},
		Env:        []string{},
		Exec:       func(path string, argv []string, env []string) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewManager() failed: %v", err)
	}
	service := NewService(repo, manager)

	target := model.ReplyTarget{Kind: model.ReplyKindDirect, UserIDs: []int64{1}}
	_, first, err := service.ScheduleRestart(ctx, model.Actor{UserID: 1}, 1, target)
	if err != nil {
		t.Fatalf("first ScheduleRestart() failed: %v", err)
	}
	if !first {
		t.Fatal("first ScheduleRestart() should return scheduled=true")
	}

	_, second, err := service.ScheduleRestart(ctx, model.Actor{UserID: 1}, 2, target)
	if err != nil {
		t.Fatalf("second ScheduleRestart() failed: %v", err)
	}
	if second {
		t.Fatal("second ScheduleRestart() should return scheduled=false (already scheduled)")
	}
}

func openLifecycleTestRepository(t *testing.T) *storage.Repository {
	t.Helper()

	repo, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "bot.sqlite3"))
	if err != nil {
		t.Fatalf("Open() failed: %v", err)
	}
	return repo
}
