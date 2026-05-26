# go-campusbot

A Zulip bot written in Go for campus/organization use. It processes Zulip direct messages as commands, manages configuration via chat, and supports graceful restarts.

## How Commands Work

Send commands as **direct messages (DMs) to the bot**. No prefix required — just type the command name.

- Stream/channel messages are **received and stored** for analysis, but are **never executed as commands**.
- Commands only execute from private/direct messages.
- Empty DMs are silently ignored.
- Unknown commands receive an "Unknown command" response.
- Malformed commands (invalid syntax) receive a "Malformed command" response.

**Examples:**

```
help
status
config list
config get restart_startup_notification
config set restart_startup_notification true
role list
role get 12345
role set 12345 admin
restart
```

## Roles and Ownership

The bot uses three roles, from lowest to highest:

| Role | Who has it | Description |
|------|-----------|-------------|
| `none` | Everyone by default | Can run `help` and `status` |
| `admin` | Explicitly assigned (stored in SQLite) | Can manage configuration and view roles |
| `owner` | The Zulip bot's creator (from `bot_owner_id`) | Full access: restart, role assignment, all admin commands |

### Bot Owner — resolved from Zulip

The bot owner is **determined automatically** at startup from the Zulip API:

1. The bot calls `GET /users/{user_id}` for its own account.
2. It reads the `bot_owner_id` field (the Zulip user ID who created the bot).
3. That user is treated as the owner for all permission checks — **in memory only**.

**The owner is never written to the database.** No bootstrapping CLI command is required.

If `bot_owner_id` is missing (the bot has no owner in Zulip), a warning is logged at startup and owner-only commands (`restart`, `role set`) are unavailable until fixed in Zulip.

If the credentials do not belong to a Zulip bot account (a regular user account), the bot refuses to start with a clear error.

### Admins — stored in SQLite

Admins are stored in the `user_roles` table. The bot owner can assign and revoke admin roles:

```
role set 12345 admin    # promote user 12345 to admin
role set 12345 none     # remove the local role row; the user falls back to none
```

Only the bot owner can set roles. Admins can view roles (`role list`, `role get`) but cannot change them.

**The `owner` role cannot be assigned through the bot.** It is always resolved from the Zulip API.

To manually add an admin (e.g., before the bot is running or for emergency access):

```sh
sqlite3 campusbot.sqlite3 \
  "INSERT OR REPLACE INTO user_roles(user_id, role, granted_by_user_id, updated_at) \
   VALUES (12345, 'admin', 0, datetime('now'));"
```

**Default role:** any user not explicitly assigned a local role has `none`. Setting a user to `none` deletes the local row rather than storing an explicit `none` row.

**Fail-closed:** if the database is unavailable, all `admin`- and `owner`-only commands are rejected. If `bot_owner_id` cannot be resolved, owner-only commands fail with "permission denied".

## Available Commands

| Command | Permission | Description |
|---------|-----------|-------------|
| `help [command]` | everyone | List commands available **to you**, or show help for one |
| `status` | everyone | Show bot uptime (admins/owners see extended details) |
| `config list` | admin | List all configuration values |
| `config get <key>` | admin | Show the value of a configuration key |
| `config set <key> <value>` | admin | Change a configuration value |
| `role list` | admin | List all explicitly assigned roles |
| `role get <user-id>` | admin | Show a user's role |
| `role set <user-id> <role>` | **owner** | Assign a role to a user (`admin` or `none` only) |
| `restart` | **owner** | Gracefully restart the bot process |

### Permission-aware help

The `help` command only shows commands the requesting user is authorised to run:

| Role | Visible commands |
|------|-----------------|
| `none` | `help`, `status` |
| `admin` | all `none` commands + `config`, `role <list\|get>` |
| `owner` | all commands, including `restart` and `role set` |

- `restart` is **never** shown to admins or non-admins.
- The `role` command shows `role set` **only** in owner help.
- If permission state is unavailable (DB outage), help fails closed and shows only public commands — it never leaks admin/owner commands.
- Direct execution is still enforced by the router regardless of what help shows.

## First Run

### 1. Build

```sh
go build -o campusbot ./cmd/campusbot
```

### 2. Configure

Create a `zuliprc` file for your bot account. Download it from your Zulip organization's bot management page (Settings → Bots → API key). The account **must be a Zulip bot** (not a regular user).

```ini
[api]
email=campusbot-bot@yourdomain.zulipchat.com
key=your-api-key
site=https://yourdomain.zulipchat.com
```

### 3. Start the bot

```sh
./campusbot run --db ./campusbot.sqlite3 --zuliprc ./zuliprc
```

Or, since `run` is the default subcommand:

```sh
./campusbot --db ./campusbot.sqlite3 --zuliprc ./zuliprc
```

The bot will:
- Connect to Zulip and fetch its own user info
- Resolve the bot owner from `bot_owner_id` via the Zulip API
- Register or resume a Zulip event queue (all event types, all public channels)
- Process commands sent as direct messages

**No manual bootstrapping is required.** The bot owner is determined automatically.

### 4. Verify startup

The bot logs something like:

```
level=INFO msg="bot owner resolved from Zulip" owner_user_id=42
level=INFO msg="zulip bot initialized" user_id=7 email=campusbot-bot@... full_name="Campus Bot"
level=INFO msg="registered Zulip event queue" queue_id=... last_event_id=...
```

The owner (user 42 in the example) can now send DMs to the bot and run any command.

### 5. Run as a systemd service

```ini
[Unit]
Description=go-campusbot Zulip bot
After=network.target

[Service]
ExecStart=/opt/campusbot/campusbot run \
    --zuliprc /etc/campusbot/zuliprc \
    --db /var/lib/campusbot/campusbot.sqlite3
Restart=on-failure
RestartSec=5s
User=campusbot
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

## Configuration Keys

| Key | Default | Description |
|-----|---------|-------------|
| `restart_startup_notification` | `true` | Send a "restart complete" message after coming back online |

Change a config value (as admin or owner):

```
config set restart_startup_notification false
```

## Database and sqlc

All Go database access goes through sqlc-generated code in `internal/zulipbot/storage/db`. SQL statements live in:

- `internal/zulipbot/storage/sql/schema.sql`
- `internal/zulipbot/storage/sql/queries.sql`

Regenerate after changing either SQL file:

```sh
make sqlc
```

Equivalent direct command:

```sh
go tool github.com/sqlc-dev/sqlc/cmd/sqlc generate
```

The SQLite schema is still development-stage. The current schema is a clean baseline for `config`, `user_roles`, `event_queue_state`, `processed_messages`, `restart_requests`, `raw_events`, and `audit_log`. Legacy role migrations and owner bootstrapping are intentionally not maintained right now; if the baseline changes, reset the development database or add a real migration policy.

## Zulip Event Queue

The bot registers a broad Zulip event queue to capture as much data as possible for analysis:

- **No `event_types` filter** — the bot receives all event types the Zulip server sends (messages, reactions, channel updates, presence, etc.).
- **`all_public_streams = true`** — the bot receives events from every public channel, not just channels it is subscribed to.
- **No `narrow` filter** — no per-stream or per-topic restriction.
- `fetch_event_types` is not set, so Zulip uses its default.
- `client_capabilities.notification_settings_null = true` is sent because some Zulip servers require it whenever `client_capabilities` is present.

Queue registration decodes only the fields the bot needs (`queue_id` and `last_event_id`) and tolerates additional response fields. This is intentional: newer Zulip servers can add fields before the Go client dependency has typed support for them.

**Receiving public channel events does not enable stream commands.** Commands are only executed from **direct messages**. Stream/channel messages are received and stored in `raw_events` for analysis only.

Private channel visibility still depends on the bot being subscribed to those channels.

## Raw Event Storage

Every non-heartbeat event received from the Zulip queue is stored in the `raw_events` SQLite table for later analysis and debugging:

| Column | Description |
|--------|-------------|
| `queue_id` | Zulip event queue ID |
| `event_id` | Zulip event ID (unique per queue) |
| `event_type` | Event type string (e.g. `message`, `reaction`) |
| `received_at` | Timestamp when the event was received |
| `raw_json` | JSON-serialized event payload |

**Heartbeat events** are not stored (they contain no useful data).  
**Duplicate events** are silently ignored (`INSERT OR IGNORE`), preserving command execution idempotency.  
**Storage failures** are logged as warnings and do not interrupt event processing or command execution.  
**Retention:** events older than 7 days or beyond 100,000 rows are cleaned up on each bot startup.

## Event Architecture

Incoming Zulip events flow through two conceptual lanes:

### Lane 1: Live Zulip Ingest (raw_events)

Every non-heartbeat event received from the Zulip long-poll queue is immediately stored in `raw_events`. This table is an append-only audit log of everything the bot has observed — useful for debugging, replaying, and analysis. Messages and heartbeats are also excluded from lane 2 but messages are still stored here.

### Lane 2: Persistent Channel Lifecycle Queue (channel_lifecycle_queue)

Channel and subscription events additionally produce entries in `channel_lifecycle_queue`. This is a persistent, replayable work queue with explicit status tracking. Workers can claim entries, mark them done/failed/skipped, and entries can be reset for replay without needing the original Zulip event queue.

**What is enqueued:**

| Lifecycle Kind | Zulip Event |
|----------------|-------------|
| `channel_created` | `stream op:create` |
| `channel_updated` | `stream op:update` |
| `channel_deleted` | `stream op:delete` |
| `subscription_added` | `subscription op:add` |
| `subscription_removed` | `subscription op:remove` |
| `subscription_peer_add` | `subscription op:peer_add` |
| `subscription_peer_remove` | `subscription op:peer_remove` |

**What is excluded from the lifecycle queue:**
- Message events (processed separately via command routing)
- Heartbeat events (contain no useful data)
- `subscription op:update` (personal notification settings only; not a channel lifecycle change)
- All other non-channel event types (presence, realm events, etc.)

**Replay behavior:**
Each entry's `payload_json` column stores the full raw JSON of the Zulip event at the time of ingestion. This makes replay self-contained — no access to the Zulip event queue or any other external system is required. Entries can be reset to `pending` individually (`ResetChannelLifecycleEntryToPending`) or in bulk for all failed entries (`ResetAllFailedChannelLifecycleEntries`).

**Deduplication:**
A `UNIQUE(zulip_event_id, lifecycle_kind)` constraint ensures that re-delivery of the same Zulip event (at-least-once delivery) never creates duplicate queue entries. The INSERT uses `INSERT OR IGNORE`.

**Current state:**
The queue is populated on every channel/subscription event but is not yet consumed by any worker. This is intentional: the queue is infrastructure for future automatic channel join/leave logic. No automatic channel management occurs yet.

**Inspecting the queue:**

```sh
# Count entries by status
sqlite3 campusbot.sqlite3 \
  "SELECT status, COUNT(*) FROM channel_lifecycle_queue GROUP BY status;"

# List pending entries
sqlite3 campusbot.sqlite3 \
  "SELECT id, lifecycle_kind, available_at, payload_json \
   FROM channel_lifecycle_queue WHERE status='pending' ORDER BY available_at LIMIT 20;"

# Reset all failed entries to pending for retry
sqlite3 campusbot.sqlite3 \
  "UPDATE channel_lifecycle_queue SET status='pending', locked_at=NULL, locked_by=NULL, \
   processed_at=NULL, last_error=NULL, available_at=datetime('now') WHERE status='failed';"
```

## Operations

### Restart behavior

When `restart` is sent as a DM (owner only):

1. The router checks owner permission — admin/none actors receive `permission denied` and the process does not restart.
2. The handler returns a confirmation message **immediately**, with scheduling deferred to the `AfterResponse` hook.
3. The event loop sends the confirmation reply to Zulip.
4. The message is marked processed in SQLite.
5. `ScheduleRestart` writes a `restart_requests` row and sets `accepting=false` in memory — the bot stops accepting new commands.
6. The event loop persists the current `last_event_id` in `event_queue_state`.
7. The loop returns `ErrRestartRequested`.
8. `main.go` catches the error and calls `app.RestartProcess()`:
   - `MarkRestartInProgress` updates the DB row.
   - `app.Close()` closes SQLite. **The Zulip event queue is preserved** (not deregistered) so the new process can resume it.
   - `syscall.Exec` replaces the current process with the same binary, same `argv`, same environment (same PID on Linux/macOS).
9. On startup, if `restart_startup_notification = true`, the new process sends "Restart complete" to the original requester.

**Why ~1 second?** The only I/O on the hot path is one Zulip network round-trip (sending the ack) plus a handful of SQLite writes. `syscall.Exec` itself is near-instant. The ~1 s observed is dominated by network latency and is expected.

**Ordering guarantees:**
- Ack is sent → message marked processed → scheduling → state persisted → exec. Nothing is skipped.
- `syscall.Exec` is a hard process replacement; Go deferred functions do not run after it. All cleanup (`Close()`) happens before `ExecRestart()`.
- Exec failure is returned as a Go error, logged by `main.go`, and results in a non-zero exit.

**Normal shutdown vs restart shutdown:**
- Normal shutdown (Ctrl-C / SIGTERM): context is cancelled, loop exits, `defer app.Close()` deregisters the Zulip queue and closes the DB.
- Restart shutdown: loop exits via `ErrRestartRequested`, `app.Close()` skips queue deregistration so the new process can resume the same queue.

### Dry-run restart (testing)

```sh
./campusbot --zuliprc ./zuliprc --dry-run-restart
```

With `--dry-run-restart`, the injected exec function prints the exec arguments and returns `nil` without replacing the process. All steps up to and including `ExecRestart()` execute normally — the only difference is the process is not replaced.

How to verify a real restart worked from logs:
```
level=INFO msg="command received" command=restart actor_user_id=42
level=INFO msg="executing requested restart"
level=INFO msg="bot owner resolved from Zulip" owner_user_id=42
level=INFO msg="zulip bot initialized" ...
level=INFO msg="resuming Zulip event queue" queue_id=... last_event_id=...
```

### Event queue and poll timeout

Each poll is bounded by `--poll-timeout` (default 90s, matching Zulip's server long-poll duration).

```sh
./campusbot --zuliprc ./zuliprc --poll-timeout 120s
```

### BAD_EVENT_QUEUE_ID recovery

If the Zulip server discards the bot's event queue (e.g. server restart), the loop registers a new queue and continues. An audit event (`event_queue.recover`) is recorded.

### Queue persistence and resume

The current queue ID and last event ID are persisted in SQLite. On restart, the bot tries to resume the existing queue. If the queue is no longer valid, it registers a new one.

## Database Schema

| Table | Purpose |
|-------|---------|
| `bot_config` | Key-value configuration |
| `user_roles` | Explicitly assigned roles (`admin` or `none` only; owner is not stored) |
| `event_queue_state` | Persisted queue ID and last event ID |
| `processed_messages` | Deduplication cache for command messages |
| `restart_requests` | Restart operation tracking |
| `audit_log` | Audit trail for privileged actions |
| `raw_events` | Raw Zulip event storage for analysis |
| `channel_lifecycle_queue` | Persistent, replayable queue for channel and subscription lifecycle events |

The `user_roles` table allows only `admin` and `none`, but the repository deletes the row when `role set <user-id> none` is used. The `owner` role is resolved from the Zulip API at runtime and never persisted.

There is a minimal `schema_migrations` table with a single development baseline row. It is not a legacy migration system; incompatible old development databases should be reset unless a real migration policy is added later.

## Integration Tests

Integration tests require a live Zulip server and are gated by a build tag. The credentials must belong to a **Zulip bot account** (not a regular user).

```sh
export CAMPUSBOT_INTEGRATION_ZULIPRC=/path/to/test-zuliprc
go test -v -tags integration ./internal/zulipbot/integration/ -timeout 30s
```

## Environment Variables

| Variable | Flag equivalent | Description |
|----------|----------------|-------------|
| `ZULIPRC` | `--zuliprc` | Path to zuliprc file |
| `CAMPUSBOT_DB_PATH` | `--db` | Path to SQLite database |

## Project Layout

```
cmd/campusbot/          - entry point (run subcommand, default)
internal/zulipbot/
  app.go                - application wiring, bot identity resolution, staticOwnerProvider
  bot.go                - Zulip client wrapper (ResolveBotIdentity, BotIdentity)
  audit/                - audit log types
  command/              - command parsing, registry, router
  configsvc/            - bot configuration service
  eventloop/            - Zulip event polling loop, broad queue registration, raw event storage
  handlers/             - command handlers (config, restart, role, status)
  integration/          - integration tests (build tag: integration)
  lifecycle/            - restart management
  model/                - shared types (Actor, ReplyTarget)
  permissions/          - role-based access control (OwnerProvider interface)
  storage/              - SQLite repository wrapper plus sqlc schema/queries/generated code
```
