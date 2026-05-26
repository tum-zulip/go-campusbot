package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/lifecycle"
)

const (
	exitSuccess = 0
	exitFailure = 1
	exitUsage   = 2

	startupTimeout = 15 * time.Second
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches to a subcommand if one is given, or falls through to runBot.
func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "run":
			// explicit "run" subcommand — strip it and continue
			args = args[1:]
		case "help", "-h", "--help":
			printTopLevelHelp()
			return exitSuccess
		}
	}
	return runBot(args)
}

// runBot is the normal bot startup path. It never mutates user roles.
func runBot(args []string) int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	flags := flag.NewFlagSet("campusbot run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)

	rcPath := flags.String("zuliprc", envOrDefault("ZULIPRC", zulipbot.DefaultRCPath), "path to zuliprc")
	dbPath := flags.String("db", envOrDefault("CAMPUSBOT_DB_PATH", zulipbot.DefaultDBPath), "path to SQLite database")
	pollTimeout := flags.Duration("poll-timeout", 90*time.Second, "HTTP timeout per Zulip event poll")
	dryRunRestart := flags.Bool("dry-run-restart", false, "log restart exec arguments without exec-ing")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitSuccess
		}
		return exitUsage
	}

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var restartExec lifecycle.ExecFunc
	if *dryRunRestart {
		restartExec = func(path string, argv []string, env []string) error {
			fmt.Fprintf(os.Stderr, "dry-run: would exec %q with argv %v\n", path, argv)
			return nil
		}
	}

	startupCtx, cancelStartup := context.WithTimeout(runCtx, startupTimeout)
	app, err := zulipbot.NewApp(startupCtx, zulipbot.RuntimeConfig{
		RCPath:      *rcPath,
		DBPath:      *dbPath,
		Logger:      logger,
		PollTimeout: *pollTimeout,
		RestartExec: restartExec,
	})
	cancelStartup()
	if err != nil {
		logger.ErrorContext(runCtx, "failed to initialize Zulip bot", "error", err)
		return exitFailure
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

	if err := app.Run(runCtx); err != nil {
		if errors.Is(err, lifecycle.ErrRestartRequested) {
			logger.InfoContext(runCtx, "executing requested restart")
			if err := app.RestartProcess(); err != nil {
				logger.ErrorContext(runCtx, "failed to restart process", "error", err)
				return exitFailure
			}
			return exitSuccess
		}
		logger.ErrorContext(runCtx, "bot stopped with error", "error", err)
		return exitFailure
	}

	return exitSuccess
}

func printTopLevelHelp() {
	fmt.Fprintln(os.Stderr, "Usage: campusbot <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Subcommands:")
	fmt.Fprintln(os.Stderr, "  run  Start the bot (default when no subcommand is given)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Run 'campusbot run -h' for subcommand flags.")
}

func envOrDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}
