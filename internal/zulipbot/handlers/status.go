package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/tum-zulip/go-zulip/zulip"

	"github.com/tum-zulip/go-campusbot/internal/zulipbot/command"
	"github.com/tum-zulip/go-campusbot/internal/zulipbot/model"
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
	Check(ctx context.Context, actor model.Actor, minRole zulip.Role) error
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
	}
}

func (handler *StatusHandler) Handle(ctx context.Context, req command.Request) (command.Result, error) {
	if len(req.Invocation.Args) != 0 {
		return command.Result{}, command.NewUserError("Usage: `status`")
	}

	uptimeSec := handler.provider.UptimeSeconds()
	hours := uptimeSec / 3600
	minutes := (uptimeSec % 3600) / 60
	seconds := uptimeSec % 60

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
		queueID, lastEventID, ok, err := handler.provider.QueueStatus(ctx)
		if err != nil {
			fmt.Fprintf(&sb, "\nqueue_status: error (%v)", err)
		} else if ok {
			fmt.Fprintf(&sb, "\nqueue_id: %s, last_event_id: %d", queueID, lastEventID)
		} else {
			fmt.Fprintf(&sb, "\nqueue_status: not registered")
		}

		if err := handler.provider.DBReachable(ctx); err != nil {
			fmt.Fprintf(&sb, "\ndb_reachable: no (%v)", err)
		} else {
			fmt.Fprintf(&sb, "\ndb_reachable: yes")
		}

		pending, err := handler.provider.RestartPending(ctx)
		if err != nil {
			fmt.Fprintf(&sb, "\nrestart_pending: error (%v)", err)
		} else {
			fmt.Fprintf(&sb, "\nrestart_pending: %v", pending)
		}
	}

	return command.Result{Content: sb.String()}, nil
}
