package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/eventloop"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/lifecycle"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/permissions"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const DefaultDBPath = "campusbot.sqlite3"

type RuntimeConfig struct {
	RCPath     string
	DBPath     string
	ClientName string
	Logger     *slog.Logger

	RestartExec lifecycle.ExecFunc
	PollTimeout time.Duration
}

type App struct {
	bot             *Bot
	repo            *storage.Repository
	config          *configsvc.Service
	lifecycle       *lifecycle.Service
	loop            *eventloop.Loop
	startupNotifier *lifecycle.StartupNotifier
	logger          *slog.Logger
	closed          atomic.Bool
	startedAt       time.Time
}

// staticOwnerProvider is an OwnerProvider backed by a fixed user ID resolved at startup.
type staticOwnerProvider struct {
	ownerUserID int64
}

func (p *staticOwnerProvider) OwnerUserID() int64 { return p.ownerUserID }

func NewApp(ctx context.Context, cfg RuntimeConfig) (*App, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath
	}

	repo, err := storage.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = repo.Close()
		}
	}()

	bot, err := New(ctx, Config{
		RCPath:     cfg.RCPath,
		ClientName: cfg.ClientName,
		Logger:     cfg.Logger,
	})
	if err != nil {
		return nil, err
	}

	identity, err := bot.ResolveBotIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bot identity: %w", err)
	}
	if !identity.IsBot {
		return nil, errors.New(
			"campusbot must run as a Zulip bot account; the configured credentials belong to a regular user account",
		)
	}
	if identity.OwnerID == 0 {
		cfg.Logger.WarnContext(
			ctx,
			"bot has no owner configured in Zulip (bot_owner_id is missing); owner-only commands (restart, role set) will be unavailable",
		)
	} else {
		cfg.Logger.InfoContext(ctx, "bot owner resolved from Zulip", "owner_user_id", identity.OwnerID)
	}

	ownerProvider := &staticOwnerProvider{ownerUserID: identity.OwnerID}

	manager, err := lifecycle.NewManager(lifecycle.ManagerConfig{Exec: cfg.RestartExec})
	if err != nil {
		return nil, err
	}
	lifecycleService := lifecycle.NewService(repo, manager)
	permissionService := permissions.NewService(repo, ownerProvider)
	configService := configsvc.NewService(repo, permissionService)

	startedAt := time.Now().UTC()

	statusProvider := &appStatusProvider{
		repo:      repo,
		lifecycle: lifecycleService,
		startedAt: startedAt,
	}
	roleService := &appRoleService{repo: repo, ownerUserID: identity.OwnerID}

	registry := command.NewRegistry()
	if err := registry.Register(command.NewHelpHandler(registry, permissionService)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewConfigHandler(configService)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewRestartHandler(lifecycleService)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewStatusHandler(statusProvider, permissionService)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewRoleHandler(roleService, permissionService)); err != nil {
		return nil, err
	}

	router, err := command.NewRouter(command.RouterConfig{
		Registry:  registry,
		Auth:      permissionService,
		Auditor:   repo,
		Accepting: lifecycleService.Accepting,
		Logger:    cfg.Logger,
	})
	if err != nil {
		return nil, err
	}

	messenger := NewMessenger(bot)
	loop, err := eventloop.New(eventloop.Config{
		Source:      eventloop.NewZulipSource(bot.Client()),
		Repo:        repo,
		Router:      router,
		Messenger:   messenger,
		Restart:     lifecycleService,
		OwnUserID:   bot.OwnUserID(),
		Logger:      cfg.Logger,
		PollTimeout: cfg.PollTimeout,
	})
	if err != nil {
		return nil, err
	}

	cleanup = false
	return &App{
		bot:             bot,
		repo:            repo,
		config:          configService,
		lifecycle:       lifecycleService,
		loop:            loop,
		startupNotifier: lifecycle.NewStartupNotifier(repo, messenger, cfg.Logger),
		logger:          cfg.Logger,
		startedAt:       startedAt,
	}, nil
}

func (app *App) Run(ctx context.Context) error {
	notify, err := app.config.Bool(ctx, configsvc.KeyRestartStartupNotification)
	if err != nil {
		return fmt.Errorf("load restart notification config: %w", err)
	}
	if notify {
		if err := app.startupNotifier.NotifyRestartComplete(ctx); err != nil {
			app.logger.WarnContext(ctx, "restart completion notification failed", "error", err)
		}
	} else if err := app.startupNotifier.MarkRestartComplete(ctx); err != nil {
		app.logger.WarnContext(ctx, "failed to mark restart complete", "error", err)
	}

	err = app.loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (app *App) Close() error {
	if app == nil || app.repo == nil {
		return nil
	}
	if !app.closed.CompareAndSwap(false, true) {
		return nil
	}
	if app.loop != nil && !app.lifecycle.RestartRequested() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.loop.DeregisterQueue(ctx); err != nil {
			app.logger.WarnContext(ctx, "failed to deregister Zulip event queue", "error", err)
		}
	}
	return app.repo.Close()
}

func (app *App) RestartProcess() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.lifecycle.MarkRestartInProgress(ctx); err != nil {
		return err
	}
	if err := app.Close(); err != nil {
		return err
	}
	return app.lifecycle.ExecRestart()
}

func (app *App) Bot() *Bot {
	return app.bot
}

// appStatusProvider implements handlers.StatusProvider using app internals.
type appStatusProvider struct {
	repo      *storage.Repository
	lifecycle *lifecycle.Service
	startedAt time.Time
}

func (p *appStatusProvider) UptimeSeconds() int64 {
	return int64(time.Since(p.startedAt).Truncate(time.Second).Seconds())
}

func (p *appStatusProvider) QueueStatus(ctx context.Context) (string, int64, bool, error) {
	state, ok, err := p.repo.EventQueueState(ctx)
	if err != nil {
		return "", 0, false, err
	}
	if !ok {
		return "", 0, false, nil
	}
	return state.QueueID, state.LastEventID, true, nil
}

func (p *appStatusProvider) DBReachable(ctx context.Context) error {
	return p.repo.Ping(ctx)
}

func (p *appStatusProvider) RestartPending(ctx context.Context) (bool, error) {
	_, ok, err := p.repo.PendingRestartRequest(ctx)
	return ok, err
}

func (p *appStatusProvider) Accepting() bool {
	return p.lifecycle.Accepting()
}

// appRoleService implements handlers.RoleService using the storage repository.
type appRoleService struct {
	repo        *storage.Repository
	ownerUserID int64
}

func (s *appRoleService) GetUserRole(ctx context.Context, userID int64) (string, bool, error) {
	if s.ownerUserID > 0 && userID == s.ownerUserID {
		return string(permissions.RoleOwner), true, nil
	}
	role, ok, err := s.repo.UserRole(ctx, userID)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	return string(role), true, nil
}

func (s *appRoleService) SetUserRole(ctx context.Context, userID int64, role string, grantedByUserID int64) error {
	parsed, err := permissions.ParseRole(role)
	if err != nil {
		return err
	}
	return s.repo.SetUserRole(ctx, userID, parsed, grantedByUserID)
}

func (s *appRoleService) ListUserRoles(ctx context.Context) ([]handlers.RoleRecord, error) {
	records, err := s.repo.ListUserRoles(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]handlers.RoleRecord, 0, len(records)+1)
	if s.ownerUserID > 0 {
		result = append(result, handlers.RoleRecord{
			UserID: s.ownerUserID,
			Role:   string(permissions.RoleOwner),
		})
	}
	for _, r := range records {
		if r.UserID == s.ownerUserID {
			continue
		}
		result = append(result, handlers.RoleRecord{
			UserID:          r.UserID,
			Role:            string(r.Role),
			GrantedByUserID: r.GrantedByUserID,
		})
	}
	return result, nil
}

// Ensure appStatusProvider satisfies the interface at compile time.
var _ handlers.StatusProvider = (*appStatusProvider)(nil)

// Ensure appRoleService satisfies the interface at compile time.
var _ handlers.RoleService = (*appRoleService)(nil)
