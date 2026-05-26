package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/eventloop"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/lifecycle"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
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
	manager, err := lifecycle.NewManager(lifecycle.ManagerConfig{Exec: cfg.RestartExec})
	if err != nil {
		return nil, err
	}
	lifecycleService := lifecycle.NewService(repo, manager)
	auth := &zulipAuthorizer{bot: bot}
	configService := configsvc.NewService(repo, auth)

	startedAt := time.Now().UTC()

	statusProvider := &appStatusProvider{
		repo:      repo,
		lifecycle: lifecycleService,
		startedAt: startedAt,
	}

	registry := command.NewRegistry()
	for _, h := range []command.Handler{
		command.NewHelpHandler(registry, auth),
		handlers.NewConfigHandler(configService),
		handlers.NewRestartHandler(lifecycleService),
		handlers.NewStatusHandler(statusProvider, auth),
	} {
		if err = registry.Register(h); err != nil {
			return nil, err
		}
	}

	router, err := command.NewRouter(command.RouterConfig{
		Registry:  registry,
		Auth:      auth,
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

// zulipAuthorizer implements command.Authorizer and command.RoleProvider
// by querying the Zulip API for user roles directly.
type zulipAuthorizer struct {
	bot *Bot
}

func (z *zulipAuthorizer) Check(ctx context.Context, actor model.Actor, minRole zulip.Role) error {
	if minRole == 0 {
		return nil
	}
	actorRole, err := z.fetchRole(ctx, actor.UserID)
	if err != nil {
		return fmt.Errorf("%w: %w", command.ErrPermissionUnavailable, err)
	}
	if actorRole <= minRole {
		return nil
	}
	return fmt.Errorf("%w", command.ErrDenied)
}

func (z *zulipAuthorizer) RoleFor(ctx context.Context, actor model.Actor) (zulip.Role, error) {
	return z.fetchRole(ctx, actor.UserID)
}

func (z *zulipAuthorizer) fetchRole(ctx context.Context, userID int64) (zulip.Role, error) {
	resp, _, err := z.bot.Client().GetUser(ctx, userID).Execute()
	if err != nil {
		return 0, fmt.Errorf("get Zulip user %d: %w", userID, err)
	}
	if resp == nil {
		return 0, fmt.Errorf("get Zulip user %d: empty response", userID)
	}
	return resp.User.Role, nil
}

// Ensure appStatusProvider satisfies the interface at compile time.
var _ handlers.StatusProvider = (*appStatusProvider)(nil)
