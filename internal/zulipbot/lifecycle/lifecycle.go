package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

var ErrRestartRequested = errors.New("restart requested")

type ExecFunc func(path string, argv []string, env []string) error

type Manager struct {
	executable string
	argv       []string
	env        []string
	exec       ExecFunc
	accepting  atomic.Bool
	requested  atomic.Bool
}

type ManagerConfig struct {
	Executable string
	Argv       []string
	Env        []string
	Exec       ExecFunc
}

func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Executable == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve executable for restart: %w", err)
		}
		cfg.Executable = executable
	}
	if len(cfg.Argv) == 0 {
		cfg.Argv = os.Args
	}
	if len(cfg.Env) == 0 {
		cfg.Env = os.Environ()
	}
	if cfg.Exec == nil {
		cfg.Exec = syscall.Exec
	}
	manager := &Manager{
		executable: cfg.Executable,
		argv:       append([]string(nil), cfg.Argv...),
		env:        append([]string(nil), cfg.Env...),
		exec:       cfg.Exec,
	}
	manager.accepting.Store(true)
	return manager, nil
}

func (manager *Manager) Accepting() bool {
	return manager.accepting.Load()
}

func (manager *Manager) RestartRequested() bool {
	return manager.requested.Load()
}

func (manager *Manager) RequestRestart() bool {
	if !manager.requested.CompareAndSwap(false, true) {
		return false
	}
	manager.accepting.Store(false)
	return true
}

func (manager *Manager) ExecRestart() error {
	if !manager.RestartRequested() {
		return errors.New("restart has not been requested")
	}
	return manager.exec(manager.executable, manager.argv, manager.env)
}

type Service struct {
	repo    *storage.Repository
	manager *Manager
}

func NewService(repo *storage.Repository, manager *Manager) *Service {
	return &Service{repo: repo, manager: manager}
}

func (service *Service) Accepting() bool {
	return service.manager.Accepting()
}

func (service *Service) RestartRequested() bool {
	return service.manager.RestartRequested()
}

func (service *Service) ScheduleRestart(
	ctx context.Context,
	actor model.Actor,
	messageID int64,
	target model.ReplyTarget,
) (int64, bool, error) {
	id, err := service.repo.CreateRestartRequest(ctx, storage.RestartRequest{
		RequestedByUserID: actor.UserID,
		RequestMessageID:  messageID,
		Target:            target,
	})
	if err != nil {
		return 0, false, err
	}
	scheduled := service.manager.RequestRestart()
	return id, scheduled, nil
}

func (service *Service) ExecRestart() error {
	return service.manager.ExecRestart()
}

func (service *Service) MarkRestartInProgress(ctx context.Context) error {
	id, ok, err := service.repo.LatestActiveRestartRequestID(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no active restart request")
	}
	return service.repo.MarkRestartInProgress(ctx, id)
}

type Messenger interface {
	SendReply(ctx context.Context, target model.ReplyTarget, content string) (int64, error)
}

type StartupNotifier struct {
	repo      *storage.Repository
	messenger Messenger
	logger    *slog.Logger
}

func NewStartupNotifier(repo *storage.Repository, messenger Messenger, logger *slog.Logger) *StartupNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &StartupNotifier{repo: repo, messenger: messenger, logger: logger}
}

func (notifier *StartupNotifier) NotifyRestartComplete(ctx context.Context) error {
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
		if err := notifier.repo.CompleteRestartRequest(ctx, request.ID, 0, sendErr.Error()); err != nil {
			notifier.logger.WarnContext(
				ctx,
				"failed to mark restart notification failure",
				"restart_request_id",
				request.ID,
				"error",
				err,
			)
		}
		return fmt.Errorf("send restart completion notification: %w", sendErr)
	}
	if err := notifier.repo.CompleteRestartRequest(ctx, request.ID, messageID, ""); err != nil {
		return err
	}
	return nil
}

func (notifier *StartupNotifier) MarkRestartComplete(ctx context.Context) error {
	request, ok, err := notifier.repo.PendingRestartRequest(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return notifier.repo.CompleteRestartRequest(ctx, request.ID, 0, "")
}
