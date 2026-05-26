package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
)

const (
	secondsPerMinute = 60
	secondsPerHour   = 60 * secondsPerMinute
)

// StatusProvider is the interface used by StatusHandler.
type StatusProvider interface {
	// UptimeSeconds returns bot uptime in whole seconds.
	UptimeSeconds() int64
	// QueueStatus returns queue_id and last_event_id. ok is false if no queue is registered.
	QueueStatus(ctx context.Context) (queueID string, lastEventID int64, ok bool, err error)
	// DBReachable returns nil if the database can be queried.
	DBReachable(ctx context.Context) error
	// RestartPending returns true if a restart has been requested but not completed.
	RestartPending(ctx context.Context) (bool, error)
	// Accepting returns true if the bot is currently accepting commands.
	Accepting() bool
}

// AdminChecker checks whether an actor has at least the given Zulip role.
type AdminChecker interface {
	Check(ctx context.Context, actor command.Actor, minRole zulip.Role) error
}

// StatusHandler handles the 'status' command.
type StatusHandler struct {
	provider  StatusProvider
	adminAuth AdminChecker
}

// NewStatusHandler creates a StatusHandler.
func NewStatusHandler(provider StatusProvider, adminAuth AdminChecker) *StatusHandler {
	return &StatusHandler{provider: provider, adminAuth: adminAuth}
}

func (handler *StatusHandler) Metadata() command.Metadata {
	return command.Metadata{
		Name:       "status",
		Summary:    "Show bot status and health information.",
		Usage:      "status",
		Permission: command.PermOpen,
		Privileged: false,
		ArgSpec:    command.NoArgs{},
	}
}

func (handler *StatusHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	uptimeSec := handler.provider.UptimeSeconds()
	hours := uptimeSec / secondsPerHour
	minutes := (uptimeSec % secondsPerHour) / secondsPerMinute
	seconds := uptimeSec % secondsPerMinute

	accepting := "yes"
	if !handler.provider.Accepting() {
		accepting = "no"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Bot status: **online**, uptime: %dh %dm %ds, accepting commands: %s",
		hours, minutes, seconds, accepting)

	// Admin-only details.
	isAdmin := handler.adminAuth.Check(ctx, req.Actor, zulip.RoleAdmin) == nil
	if isAdmin {
		handler.writeAdminStatus(ctx, &sb)
	}

	return command.Result{Content: sb.String()}, nil
}

func (handler *StatusHandler) writeAdminStatus(ctx context.Context, sb *strings.Builder) {
	handler.writeQueueStatus(ctx, sb)
	handler.writeDBStatus(ctx, sb)
	handler.writeRestartStatus(ctx, sb)
}

func (handler *StatusHandler) writeQueueStatus(ctx context.Context, sb *strings.Builder) {
	queueID, lastEventID, ok, err := handler.provider.QueueStatus(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(sb, "\nqueue_status: error (%v)", err)
	case ok:
		fmt.Fprintf(sb, "\nqueue_id: %s, last_event_id: %d", queueID, lastEventID)
	default:
		fmt.Fprintf(sb, "\nqueue_status: not registered")
	}
}

func (handler *StatusHandler) writeDBStatus(ctx context.Context, sb *strings.Builder) {
	if err := handler.provider.DBReachable(ctx); err != nil {
		fmt.Fprintf(sb, "\ndb_reachable: no (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\ndb_reachable: yes")
}

func (handler *StatusHandler) writeRestartStatus(ctx context.Context, sb *strings.Builder) {
	pending, err := handler.provider.RestartPending(ctx)
	if err != nil {
		fmt.Fprintf(sb, "\nrestart_pending: error (%v)", err)
		return
	}
	fmt.Fprintf(sb, "\nrestart_pending: %v", pending)
}
