package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tum-zulip/go-zulip/zulip"
	zulipclient "github.com/tum-zulip/go-zulip/zulip/client"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot"
	storagedb "github.com/tum-zulip/go-campusbot/internal/zulipbot/storage/db"
	"github.com/tum-zulip/go-campusbot/internal/zulipcache"
)

const (
	exitSuccess = 0
	exitFailure = 1

	defaultRCPath = "zuliprc"
	defaultDBPath = "campusbot.sqlite3"

	startupTimeout     = 15 * time.Second
	restartMarkTimeout = 5 * time.Second

	zulipClientMaxRetries = 32
)

type execFunc func(path string, argv []string, env []string) error

var (
	zuliprc       = envOrDefault("ZULIPRC", defaultRCPath)
	dbPath        = envOrDefault("CAMPUSBOT_DB_PATH", defaultDBPath)
	dryRunRestart bool
	logLevel      = envOrDefault("CAMPUSBOT_LOG_LEVEL", "info")
	logFormat     = envOrDefault("CAMPUSBOT_LOG_FORMAT", "text")
)

func init() {
	flag.StringVar(&zuliprc, "zuliprc", zuliprc, "path to zuliprc")
	flag.StringVar(&dbPath, "db", dbPath, "path to SQLite database")
	flag.BoolVar(&dryRunRestart, "dry-run-restart", false, "log restart exec arguments without exec-ing")
	flag.StringVar(&logLevel, "log-level", logLevel, "log level: verbose, debug, info, warn, error")
	flag.StringVar(&logFormat, "log-format", logFormat, "log format: text, json")
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

	logConfig, err := parseLogLevel(logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	parsedLogFormat, err := parseLogFormat(logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	logger := setupLogger(os.Stderr, logConfig.BotLevel, parsedLogFormat)
	zulipLogger := newLogger(os.Stderr, logConfig.ZulipClientLevel, parsedLogFormat)

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startupCtx, cancelStartup := context.WithTimeout(runCtx, startupTimeout)
	userGroupsCache := zulipcache.NewUserGroups(zulipcache.DefaultUserGroupsTTL)
	streamsCache := zulipcache.NewStreams(zulipcache.DefaultStreamsTTL)
	client, err := newZulipClient(zuliprc, zulipLogger, userGroupsCache, streamsCache)
	if err != nil {
		cancelStartup()
		logger.ErrorContext(runCtx, "failed to create Zulip client", "error", err)
		os.Exit(exitFailure)
	}
	if err := userGroupsCache.Start(runCtx, client, logger); err != nil {
		cancelStartup()
		logger.ErrorContext(runCtx, "failed to start user-groups cache", "error", err)
		os.Exit(exitFailure)
	}
	if err := streamsCache.Start(runCtx, client, logger); err != nil {
		cancelStartup()
		_ = userGroupsCache.Close()
		logger.ErrorContext(runCtx, "failed to start streams cache", "error", err)
		os.Exit(exitFailure)
	}
	db, err := openDatabase(dbPath)
	if err != nil {
		cancelStartup()
		_ = streamsCache.Close()
		_ = userGroupsCache.Close()
		logger.ErrorContext(runCtx, "failed to open database", "error", err)
		os.Exit(exitFailure)
	}
	if err := storagedb.ConfigureSQLite(startupCtx, db); err != nil {
		cancelStartup()
		_ = streamsCache.Close()
		_ = userGroupsCache.Close()
		_ = db.Close()
		logger.ErrorContext(runCtx, "failed to configure database", "error", err)
		os.Exit(exitFailure)
	}
	if err := storagedb.InitSchema(startupCtx, db); err != nil {
		cancelStartup()
		_ = streamsCache.Close()
		_ = userGroupsCache.Close()
		_ = db.Close()
		logger.ErrorContext(runCtx, "failed to initialize database schema", "error", err)
		os.Exit(exitFailure)
	}
	queries := storagedb.New(db)
	bot, err := zulipbot.NewBot(
		startupCtx,
		zulipbot.RuntimeConfig{
			Logger:     logger,
			RunContext: runCtx,
		},
		client,
		db,
		queries,
	)
	cancelStartup()
	if err != nil {
		_ = streamsCache.Close()
		_ = userGroupsCache.Close()
		_ = db.Close()
		logger.ErrorContext(runCtx, "failed to initialize Zulip bot", "error", err)
		os.Exit(exitFailure)
	}
	defer func() {
		if err := streamsCache.Close(); err != nil {
			logger.Warn("failed to close streams cache", "error", err)
		}
		if err := userGroupsCache.Close(); err != nil {
			logger.Warn("failed to close user-groups cache", "error", err)
		}
		if err := bot.Close(); err != nil {
			logger.Warn("failed to close bot", "error", err)
		}
		if err := db.Close(); err != nil {
			logger.Warn("failed to close database", "error", err)
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

func newZulipClient(
	rcPath string,
	logger *slog.Logger,
	userGroupsCache *zulipcache.UserGroups,
	streamsCache *zulipcache.Streams,
) (zulipclient.Client, error) {
	rc, err := zulip.NewZulipRCFromFile(rcPath)
	if err != nil {
		return nil, fmt.Errorf("load Zulip config %q: %w", rcPath, err)
	}
	client, err := zulipclient.NewClient(
		rc,
		zulipclient.WithClientName(zulipbot.DefaultClientName),
		zulipclient.WithLogger(logger),
		zulipclient.WithHTTPClient(newRetryableHTTPClient(rc, userGroupsCache, streamsCache)),
		zulipclient.WithMaxRetries(zulipClientMaxRetries),
		zulipclient.SkipWarnOnInsecureTLS(),
	)
	if err != nil {
		return nil, fmt.Errorf("create Zulip client: %w", err)
	}
	return client, nil
}

func newRetryableHTTPClient(
	rc *zulip.RC,
	userGroupsCache *zulipcache.UserGroups,
	streamsCache *zulipcache.Streams,
) *http.Client {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{
			Transport: userGroupsCache.RoundTripper(streamsCache.RoundTripper(resettableBodyTransport{
				base: http.DefaultTransport,
			})),
		}
	}
	transport := defaultTransport.Clone()
	if rc.Insecure != nil && *rc.Insecure {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.InsecureSkipVerify = true
	}
	return &http.Client{
		Transport: userGroupsCache.RoundTripper(streamsCache.RoundTripper(resettableBodyTransport{base: transport})),
	}
}

type resettableBodyTransport struct {
	base http.RoundTripper
}

func (t resettableBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
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

type logConfig struct {
	BotLevel         slog.Level
	ZulipClientLevel slog.Level
}

type logFormatConfig int

const (
	logFormatText logFormatConfig = iota
	logFormatJSON
)

func parseLogLevel(s string) (logConfig, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "verbose":
		return logConfig{BotLevel: slog.LevelDebug, ZulipClientLevel: slog.LevelDebug}, nil
	case "debug":
		return logConfig{BotLevel: slog.LevelDebug, ZulipClientLevel: slog.LevelInfo}, nil
	case "info":
		return logConfig{BotLevel: slog.LevelInfo, ZulipClientLevel: slog.LevelInfo}, nil
	case "warn", "warning":
		return logConfig{BotLevel: slog.LevelWarn, ZulipClientLevel: slog.LevelWarn}, nil
	case "error":
		return logConfig{BotLevel: slog.LevelError, ZulipClientLevel: slog.LevelError}, nil
	default:
		return logConfig{}, fmt.Errorf("unknown log level %q (want verbose, debug, info, warn, or error)", s)
	}
}

func parseLogFormat(s string) (logFormatConfig, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "text":
		return logFormatText, nil
	case "json":
		return logFormatJSON, nil
	default:
		return logFormatText, fmt.Errorf("unknown log format %q (want text or json)", s)
	}
}

func setupLogger(w io.Writer, level slog.Level, format logFormatConfig) *slog.Logger {
	logger := newLogger(w, level, format)
	slog.SetDefault(logger)
	return logger
}

func newLogger(w io.Writer, level slog.Level, format logFormatConfig) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch format {
	case logFormatText:
		handler = slog.NewTextHandler(w, opts)
	case logFormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
