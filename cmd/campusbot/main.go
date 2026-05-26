package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/storage"
)

const (
	exitSuccess = 0
	exitFailure = 1

	defaultRCPath = "zuliprc"
	defaultDBPath = "campusbot.sqlite3"

	startupTimeout     = 15 * time.Second
	restartMarkTimeout = 5 * time.Second
)

type execFunc func(path string, argv []string, env []string) error

var (
	zuliprc       = envOrDefault("ZULIPRC", defaultRCPath)
	dbPath        = envOrDefault("CAMPUSBOT_DB_PATH", defaultDBPath)
	dryRunRestart bool
	logLevel      = envOrDefault("CAMPUSBOT_LOG_LEVEL", "info")
)

func init() {
	flag.StringVar(&zuliprc, "zuliprc", zuliprc, "path to zuliprc")
	flag.StringVar(&dbPath, "db", dbPath, "path to SQLite database")
	flag.BoolVar(&dryRunRestart, "dry-run-restart", false, "log restart exec arguments without exec-ing")
	flag.StringVar(&logLevel, "log-level", logLevel, "log level: debug, info, warn, error")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: campusbot [run] [flags]\n\n")
		fmt.Fprintln(flag.CommandLine.Output(), "Start the Zulip campus bot.")
		fmt.Fprintln(flag.CommandLine.Output(), "\nFlags:")
		flag.PrintDefaults()
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "run" {
		os.Args = append([]string{os.Args[0]}, os.Args[2:]...)
	}
	flag.Parse()

	level, err := parseLogLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := setupLogger(os.Stderr, level)

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancelStartup := context.WithTimeout(runCtx, startupTimeout)
	client, err := newZulipClient(zuliprc, logger)
	if err != nil {
		cancelStartup()
		logger.ErrorContext(runCtx, "failed to create Zulip client", "error", err)
		os.Exit(exitFailure)
	}
	db, err := openDatabase(dbPath)
	if err != nil {
		cancelStartup()
		logger.ErrorContext(runCtx, "failed to open database", "error", err)
		os.Exit(exitFailure)
	}
	repo, err := storage.New(startupCtx, db)
	if err != nil {
		cancelStartup()
		_ = db.Close()
		logger.ErrorContext(runCtx, "failed to open database", "error", err)
		os.Exit(exitFailure)
	}
	bot, err := zulipbot.NewBot(
		startupCtx,
		zulipbot.RuntimeConfig{
			Logger:     logger,
			RunContext: runCtx,
		},
		client,
		repo,
	)
	cancelStartup()
	if err != nil {
		_ = repo.Close()
		logger.ErrorContext(runCtx, "failed to initialize Zulip bot", "error", err)
		os.Exit(exitFailure)
	}
	defer func() {
		if err := bot.Close(); err != nil {
			logger.Warn("failed to close bot", "error", err)
		}
	}()

	ownUser := bot.OwnUser()
	logger.InfoContext(runCtx, "zulip bot initialized",
		"user_id", ownUser.UserID,
		"email", ownUser.Email,
		"full_name", ownUser.FullName,
	)

	restartRequested, err := bot.Run(runCtx)
	if err != nil {
		logger.ErrorContext(runCtx, "bot stopped with error", "error", err)
		os.Exit(exitFailure)
	}
	if !restartRequested {
		os.Exit(exitSuccess)
	}

	logger.InfoContext(runCtx, "executing requested restart")
	if restartErr := restartProcess(runCtx, bot, restartExec(dryRunRestart)); restartErr != nil {
		logger.ErrorContext(runCtx, "failed to restart process", "error", restartErr)
		os.Exit(exitFailure)
	}
}

func newZulipClient(rcPath string, logger *slog.Logger) (zulipclient.Client, error) {
	rc, err := zulip.NewZulipRCFromFile(rcPath)
	if err != nil {
		return nil, fmt.Errorf("load Zulip config %q: %w", rcPath, err)
	}
	client, err := zulipclient.NewClient(
		rc,
		zulipclient.WithClientName(zulipbot.DefaultClientName),
		zulipclient.WithLogger(logger),
	)
	if err != nil {
		return nil, fmt.Errorf("create Zulip client: %w", err)
	}
	return client, nil
}

func openDatabase(path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("database path must not be empty")
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func restartProcess(ctx context.Context, bot *zulipbot.Bot, exec execFunc) error {
	markCtx, cancel := context.WithTimeout(ctx, restartMarkTimeout)
	defer cancel()
	if err := bot.MarkRestartInProgress(markCtx); err != nil {
		return err
	}
	if err := bot.Close(); err != nil {
		return err
	}
	return execRestart(exec)
}

func restartExec(dryRun bool) execFunc {
	if !dryRun {
		return syscall.Exec
	}
	return func(path string, argv []string, _ []string) error {
		fmt.Fprintf(os.Stderr, "dry-run: would exec %q with argv %v\n", path, argv)
		return nil
	}
}

func execRestart(exec execFunc) error {
	if exec == nil {
		exec = syscall.Exec
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable for restart: %w", err)
	}
	return exec(executable, os.Args, os.Environ())
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn, or error)", s)
	}
}

func setupLogger(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
