package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

type RuntimeConfig struct {
	Logger      *slog.Logger
	PollTimeout time.Duration
}

type App struct {
	bot       *Bot
	repo      *storage.Repository
	config    *configsvc.Service
	restart   *restartState
	loop      *Loop
	messenger Messenger
	logger    *slog.Logger
	closed    atomic.Bool
	startedAt time.Time
}

const closeDeregisterTimeout = 5 * time.Second

func NewApp(ctx context.Context, cfg RuntimeConfig, client zulipclient.Client, repo *storage.Repository) (*App, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if repo == nil {
		return nil, errors.New("storage repository must not be nil")
	}

	bot, err := New(ctx, client)
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

	restart := newRestartState()
	configService := configsvc.NewService(repo, bot)

	startedAt := time.Now().UTC()

	app := &App{
		bot:       bot,
		repo:      repo,
		config:    configService,
		restart:   restart,
		messenger: bot,
		logger:    cfg.Logger,
		startedAt: startedAt,
	}

	router, err := app.initCommands(configService)
	if err != nil {
		return nil, err
	}

	loop, err := NewLoop(LoopConfig{
		Source:           NewZulipSource(bot.Client()),
		Repo:             repo,
		Router:           router,
		Messenger:        bot,
		RestartRequested: restart.RestartRequested,
		OwnUserID:        bot.OwnUserID(),
		Logger:           cfg.Logger,
		PollTimeout:      cfg.PollTimeout,
	})
	if err != nil {
		return nil, err
	}

	app.loop = loop

	return app, nil
}

func (app *App) initCommands(configService *configsvc.Service) (*command.Router, error) {
	registry := command.NewRegistry()
	if err := registry.Register(command.NewHelpHandler(registry, app.bot)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewConfigHandler(configService)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewRestartHandler(app)); err != nil {
		return nil, err
	}
	if err := registry.Register(handlers.NewStatusHandler(app, app.bot)); err != nil {
		return nil, err
	}

	return command.NewRouter(command.RouterConfig{
		Registry:  registry,
		Auth:      app.bot,
		Auditor:   app.repo,
		Accepting: app.restart.Accepting,
		Logger:    app.logger,
	})
}

func (app *App) Run(ctx context.Context) (bool, error) {
	notify, err := app.config.Bool(ctx, configsvc.KeyRestartStartupNotification)
	if err != nil {
		return false, fmt.Errorf("load restart notification config: %w", err)
	}
	if notify {
		if notifyErr := app.NotifyRestartComplete(ctx); notifyErr != nil {
			app.logger.WarnContext(ctx, "restart completion notification failed", "error", notifyErr)
		}
	} else if markErr := app.MarkRestartComplete(ctx); markErr != nil {
		app.logger.WarnContext(ctx, "failed to mark restart complete", "error", markErr)
	}

	restartRequested, err := app.loop.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return false, nil
	}
	return restartRequested, err
}

func (app *App) Close() error {
	if app == nil || app.repo == nil {
		return nil
	}
	if !app.closed.CompareAndSwap(false, true) {
		return nil
	}
	if app.loop != nil && !app.restart.RestartRequested() {
		ctx, cancel := context.WithTimeout(context.Background(), closeDeregisterTimeout)
		defer cancel()
		if err := app.loop.DeregisterQueue(ctx); err != nil {
			app.logger.WarnContext(ctx, "failed to deregister Zulip event queue", "error", err)
		}
	}
	return app.repo.Close()
}

func (app *App) Bot() *Bot {
	return app.bot
}

// UptimeSeconds implements handlers.StatusProvider.
func (app *App) UptimeSeconds() int64 {
	return int64(time.Since(app.startedAt).Truncate(time.Second).Seconds())
}

// QueueStatus implements handlers.StatusProvider.
func (app *App) QueueStatus(ctx context.Context) (string, int64, bool, error) {
	state, ok, err := app.repo.EventQueueState(ctx)
	if err != nil {
		return "", 0, false, err
	}
	if !ok {
		return "", 0, false, nil
	}
	return state.QueueID, state.LastEventID, true, nil
}

// DBReachable implements handlers.StatusProvider.
func (app *App) DBReachable(ctx context.Context) error {
	return app.repo.Ping(ctx)
}

// RestartPending implements handlers.StatusProvider.
func (app *App) RestartPending(ctx context.Context) (bool, error) {
	_, ok, err := app.repo.PendingRestartRequest(ctx)
	return ok, err
}

// Accepting implements handlers.StatusProvider.
func (app *App) Accepting() bool {
	return app.restart.Accepting()
}

// RestartRequested reports whether a restart was scheduled.
func (app *App) RestartRequested() bool {
	if app == nil || app.restart == nil {
		return false
	}
	return app.restart.RestartRequested()
}

var (
	_ handlers.StatusProvider = (*App)(nil)
	_ handlers.RestartService = (*App)(nil)
)
