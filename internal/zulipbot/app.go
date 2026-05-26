package zulipbot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/channelgroup"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/announcement"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/configsvc"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/handlers"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const DefaultDBPath = "campusbot.sqlite3"

type RuntimeConfig struct {
	RCPath      string
	DBPath      string
	ClientName  string
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

	bot, err := New(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// identity, err := bot.ResolveBotIdentity(ctx)
	//if err != nil {
	//return nil, fmt.Errorf("resolve bot identity: %w", err)
	//	}
	//if !identity.IsBot {
	//	return nil, errors.New(
	//		"campusbot must run as a Zulip bot account; the configured credentials belong to a regular user account",
	//	)
	//}

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

	// Set up channelgroup client using the shared SQLite database.
	channelGroupClient := channelgroup.NewClient(bot.Client(), repo.DB())
	groupService := channelgroup.NewGroupService(channelGroupClient)

	// Set up announcement manager.
	announcementManager := announcement.NewManager(repo, bot, cfg.Logger)

	// Build group config reader adapter.
	groupConfigReader := handlers.NewGroupConfigAdapter(
		func(ctx context.Context) (int64, bool, error) {
			v, err := configService.GetRaw(ctx, configsvc.KeyAnnouncementChannelID)
			if err != nil {
				return 0, false, err
			}
			if v.IsDefault || v.Value == "" {
				return 0, false, nil
			}
			id, err := strconv.ParseInt(v.Value, 10, 64)
			if err != nil {
				return 0, false, err
			}
			return id, true, nil
		},
		func(ctx context.Context) (string, bool, error) {
			v, err := configService.GetRaw(ctx, configsvc.KeyAnnouncementTopic)
			if err != nil {
				return "", false, err
			}
			if v.IsDefault || v.Value == "" {
				return "", false, nil
			}
			return v.Value, true, nil
		},
	)

	registry := command.NewRegistry()
	for _, h := range []command.Handler{
		command.NewHelpHandler(registry, bot),
		handlers.NewConfigHandler(configService),
		handlers.NewRestartHandler(app),
		handlers.NewStatusHandler(app, bot),
		handlers.NewGroupHandler(groupService, groupService, repo, repo, announcementManager, repo, groupConfigReader, bot),
	} {
		if err = registry.Register(h); err != nil {
			return nil, err
		}
	}

	router, err := command.NewRouter(command.RouterConfig{
		Registry:  registry,
		Auth:      bot,
		Auditor:   repo,
		Accepting: restart.Accepting,
		Logger:    cfg.Logger,
	})
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
		GroupSubscriber:  groupService,
	})
	if err != nil {
		return nil, err
	}

	app.loop = loop

	cleanup = false
	return app, nil
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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

var (
	_ handlers.StatusProvider = (*App)(nil)
	_ handlers.RestartService = (*App)(nil)
)
