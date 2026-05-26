package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
)

const (
	exitSuccess = 0
	exitFailure = 1

	startupTimeout     = 15 * time.Second
	restartMarkTimeout = 5 * time.Second
)

type execFunc func(path string, argv []string, env []string) error

var (
	zuliprc       = envOrDefault("ZULIPRC", zulipbot.DefaultRCPath)
	dbPath        = envOrDefault("CAMPUSBOT_DB_PATH", zulipbot.DefaultDBPath)
	pollTimeout   = 90 * time.Second
	dryRunRestart bool
	logLevel      = envOrDefault("CAMPUSBOT_LOG_LEVEL", "info")
)

func init() {
	flag.StringVar(&zuliprc, "zuliprc", zuliprc, "path to zuliprc")
	flag.StringVar(&dbPath, "db", dbPath, "path to SQLite database")
	flag.DurationVar(&pollTimeout, "poll-timeout", pollTimeout, "HTTP timeout per Zulip event poll")
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
	app, err := zulipbot.NewApp(startupCtx, zulipbot.RuntimeConfig{
		RCPath:      zuliprc,
		DBPath:      dbPath,
		Logger:      logger,
		PollTimeout: pollTimeout,
	})
	cancelStartup()
	if err != nil {
		logger.ErrorContext(runCtx, "failed to initialize Zulip bot", "error", err)
		os.Exit(exitFailure)
	}
	defer func() {
		if err := app.Close(); err != nil {
			logger.Warn("failed to close app", "error", err)
		}
	}()

	ownUser := app.Bot().OwnUser()
	logger.InfoContext(runCtx, "zulip bot initialized",
		"user_id", ownUser.UserID,
		"email", ownUser.Email,
		"full_name", ownUser.FullName,
	)

	restartRequested, err := app.Run(runCtx)
	if err != nil {
		logger.ErrorContext(runCtx, "bot stopped with error", "error", err)
		os.Exit(exitFailure)
	}
	if !restartRequested {
		os.Exit(exitSuccess)
	}

	logger.InfoContext(runCtx, "executing requested restart")
	if restartErr := restartProcess(runCtx, app, restartExec(dryRunRestart)); restartErr != nil {
		logger.ErrorContext(runCtx, "failed to restart process", "error", restartErr)
		os.Exit(exitFailure)
	}
}

func restartProcess(ctx context.Context, app *zulipbot.App, exec execFunc) error {
	markCtx, cancel := context.WithTimeout(ctx, restartMarkTimeout)
	defer cancel()
	if err := app.MarkRestartInProgress(markCtx); err != nil {
		return err
	}
	if err := app.Close(); err != nil {
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
