# go-campusbot

A Zulip bot written in Go for campus/organization use. It processes Zulip direct messages as commands, manages configuration via chat, and supports channel group subscription management with reaction-based UI.

## How Commands Work

Send commands as **direct messages (DMs) to the bot**. No prefix required — just type the command name.

- Stream/channel messages are **received** but **never executed as commands**.
- Commands only execute from private/direct messages.
- Empty DMs are silently ignored.
- Unknown commands receive an "Unknown command" response.
- Malformed commands (invalid syntax) receive a "Malformed command" response.

## Available Commands

| Command | Permission | Description |
|---------|-----------|-------------|
| `help [command]` | everyone | List commands available to you, or show help for one |
| `status` | everyone | Show bot uptime (admins see extended details) |
| `group subscribe <short_name>` | everyone | Subscribe to a channel group |
| `group unsubscribe <short_name>` | everyone | Unsubscribe from a channel group (leaves existing channels) |
| `group unsubscribe -k <short_name>` | everyone | Unsubscribe from future group updates (keeps existing channels) |
| `group available` | admin | List Zulip user groups visible to the bot account. The IDs shown here are what you pass to `group mapping set`. |
| `group mapping list` | admin | List all emoji→group mappings |
| `group mapping set <name> <display> <zulip_user_group_id> <emoji>` | admin | Add or update an emoji→group mapping. Auto-imports the Zulip user group locally on first use. |
| `group mapping disable <name>` | admin | Disable an emoji→group mapping |
| `group announce` | admin | Send or update the channel group announcement message |
| `group announce set-message <id>` | admin | Migrate: register an existing Zulip message as the announcement |
| `group announce inspect` | admin | Show announcement configuration and current state |
| `config list` | admin | List all configuration values |
| `config get <key>` | admin | Show the value of a configuration key |
| `config set <key> <value>` | admin | Change a configuration value |
| `restart` | **owner** | Gracefully restart the bot process |

## Channel Group Subscription

### Overview

The bot manages *channel groups* — logical collections of Zulip channels backed by a Zulip user group. Subscribing to a channel group:

1. Subscribes the user to every current channel in the group.
2. Adds the user to the backing Zulip user group, so they are **automatically subscribed to future channels** added to the group.

Unsubscribing removes the user from the user group (stopping future auto-subscriptions) **and** unsubscribes them from current group channels (unless `-k` is used).

### Reaction-based subscription

The bot maintains a single **announcement message** in a configured Zulip channel. The message lists available channel groups with their emoji icons. Users react to subscribe or unsubscribe:

- ✅ **React with a group's emoji** → subscribed to that channel group
- ❌ **Remove your reaction** → unsubscribed from that group and its current channels

The bot posts its own reactions on the announcement to serve as a visual palette.

### PM fallback commands

If reactions don't work, users can write DMs to the bot:

```
group subscribe WI
group unsubscribe WI
group unsubscribe -k WI   # leave the group but keep existing channel memberships
```

`<short_name>` is the identifier for the channel group (e.g. `WI` for Wirtschaftsinformatik).

### Announcement message format

The announcement looks like:

```
Hi! :bothappy:

I have the pleasure to announce some channel groups here.

You may subscribe to a channel group in order to be automatically subscribed to all channels
belonging to that group. Also, you will be kept updated when new channels are added to the group.

Just react to this message with the emoji of the channel group you like to subscribe to.
Remove your emoji to unsubscribe from this group. (1, 2)

| Course | Emoji |   | Course | Emoji |   | Course | Emoji |
| --- | --- | --- | --- | --- | --- | --- | --- |
| Informatik | :computer: |   | Wirtschaftsinformatik | :chart_with_upwards_trend: |   | ...

In case the emojis do not work for you, you may write me a PM:
- `group subscribe <course_short_name>`
- `group unsubscribe <course_short_name>`

Have a nice day! :bothappypad:

(1) Note that this will also unsubscribe you from the existing channels of this group.
    If you only want to cancel the subscription without being unsubscribed from existing
    channels, just write me a PM:
    - `group unsubscribe -k <course_short_name>`

(2) If your course has changed its emote, remove your reaction of the old emote and react
    with the new one. Then, you can remove the new reaction again to unsubscribe.
```

The table is generated from enabled emoji→group mappings in the database. It uses a 3-column-group layout. The message is edited in-place whenever mappings change (identified by content hash).

### Setup procedure

**1. Discover Zulip user group IDs**

The backing Zulip user groups must already exist. Their integer IDs are needed when configuring mappings. Run `group available` to query Zulip live for the user groups the bot account can see:

```
group available
```

Sample output:

```
Zulip-visible user groups (use the id with `group mapping set`):
- id=1001 **Informatik** (87 members) — Informatik students
- id=1002 **Wirtschaftsinformatik** (42 members)
```

These IDs are what you pass directly to `group mapping set`. The bot manages the local channelgroup record itself — admins don't need to import groups separately.

**2. Configure the announcement channel and topic**

```
config set announcement.channel_id 42
config set announcement.topic "Channel Groups"
```

**3. Add emoji→group mappings**

```
group mapping set INF "Informatik" 1001 computer
group mapping set WI "Wirtschaftsinformatik" 1002 chart_with_upwards_trend
```

Arguments: `<short_name> <display_name> <zulip_user_group_id> <emoji_name>`

- `short_name` — identifier used in PM commands (e.g. `WI`)
- `display_name` — human-readable name shown in the announcement table
- `zulip_user_group_id` — Zulip user group ID, as shown by `group available`
- `emoji_name` — Zulip emoji name without colons (e.g. `computer`)

If the Zulip user group has not yet been tracked locally, `group mapping set` auto-imports it before saving the mapping; the success message indicates this happened. If the ID is not visible to the bot in Zulip, the command fails with

```
Channel group <id> is not visible in Zulip. Run `group available` to see available groups.
```

No mapping is written and no announcement update is triggered when the import or visibility check fails.

To see the configured mappings:

```
group mapping list
```

**4. Send the announcement**

```
group announce
```

This sends a new message to the configured channel/topic (or edits the existing one), adds bot reactions, and stores the message ID for future updates.

`group announce` refuses to publish if any **enabled** mapping references a missing channel group. The error lists the offending mappings, e.g.

```
Cannot update announcement: enabled mapping(s) reference missing channel group(s): PGDP -> channel_group_id=30. Disable or fix the mapping, or create/import the channel group first.
```

Either disable the mapping (`group mapping disable <short_name>`) or re-run `group mapping set` with a valid `zulip_user_group_id` (the bot auto-imports as needed), then re-run `group announce`. Disabled mappings with missing channel groups do not block publication.

`group mapping list` annotates rows whose channel group is missing with `[missing channel group]`, making it easy to find broken configuration.

**5. Mappings update automatically**

When you run `group mapping set` or `group mapping disable`, the announcement message is edited automatically if a stored message ID or a configured channel/topic is available.

### Migrating from an existing announcement message

If a bot instance already manages an announcement message (e.g. from a previous deployment), you can register the existing message ID instead of creating a new one:

```
group announce set-message <message_id>
```

After setting the message ID, run `group announce` to re-render the content and add bot reactions. The `announcement.channel_id` and `announcement.topic` config keys are **not required** when a message ID is already stored — the bot edits the existing message directly.

To check the current announcement configuration:

```
group announce inspect
```

This shows the stored message ID (if any), the current mode (edit vs. create), and the configured channel/topic values.

### Disabling a mapping

```
group mapping disable WI
```

The mapping becomes invisible in the announcement and reaction events for it are ignored. The bot does not remove its own emoji reaction (Zulip does not allow removing another user's reaction; the bot can only remove its own reactions, which it added separately).

## Roles and Ownership

The bot uses Zulip's built-in organizational roles:

| Role | Permission level |
|------|-----------------|
| `none`/member | Can run `help`, `status`, `group subscribe/unsubscribe` |
| `admin` | All of the above + `config`, `group mapping`, `group announce` |
| `owner` | All commands, including `restart` |

The **bot owner** is resolved dynamically from the Zulip API (`bot_owner_id` field on the bot's own user record). It is never stored in the database.

**Fail-closed:** if the Zulip API is unavailable during a permission check, privileged commands are rejected.

## First Run

### 1. Build

```sh
go build -o campusbot ./cmd/campusbot
```

### 2. Configure

Create a `zuliprc` file for your bot account (download from Zulip Settings → Bots → API key). The account **must be a Zulip bot**, not a regular user.

```ini
[api]
email=campusbot-bot@yourdomain.zulipchat.com
key=your-api-key
site=https://yourdomain.zulipchat.com
```

### 3. Start the bot

```sh
./campusbot --db ./campusbot.sqlite3 --zuliprc ./zuliprc
```

The bot will:
- Connect to Zulip and fetch its own user info
- Resolve the bot owner from `bot_owner_id` via the Zulip API
- Register or resume a Zulip event queue (all event types, all public channels)
- Process commands sent as direct messages

### 4. Systemd service

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

[Install]
WantedBy=multi-user.target
```

## Configuration Keys

| Key | Default | Description |
|-----|---------|-------------|
| `restart_startup_notification` | `true` | Send a "restart complete" DM after coming back online |
| `announcement.channel_id` | _(unset)_ | Channel ID for the channel group announcement message |
| `announcement.topic` | _(unset)_ | Topic for the channel group announcement message |

## Database and sqlc

All Go database access goes through sqlc-generated code in `internal/zulipbot/storage/db`. SQL statements live in:

- `internal/zulipbot/storage/sql/schema.sql`
- `internal/zulipbot/storage/sql/queries.sql`

The channelgroup tables (`channel_groups`, `channel_group_channels`) are also created by the bot's schema (single SQLite file).

Regenerate after changing SQL files:

```sh
make generate
```

## Database Schema

| Table | Purpose |
|-------|---------|
| `bot_config` | Key-value configuration (config command) |
| `event_queue_state` | Persisted Zulip event queue ID and last event ID |
| `processed_messages` | Deduplication cache for command DMs |
| `restart_requests` | Restart operation tracking |
| `audit_log` | Audit trail for privileged actions |
| `channel_groups` | Channel groups (id = Zulip user group ID) |
| `channel_group_channels` | Channels belonging to each group |
| `emoji_group_mappings` | Emoji → channel group mappings for the announcement |
| `announcement_state` | Stored announcement message ID and content hash |
| `processed_reactions` | Deduplication for reaction events (restart-safe) |

## Zulip Event Queue

The bot registers a broad event queue:

- **No `event_types` filter** — receives all event types including `reaction`
- **`all_public_streams = true`** — events from every public channel
- Queue ID and last event ID are persisted in SQLite for restart-safe resumption

Reaction events are handled when they match the stored announcement message ID; all other reaction events are silently ignored.

## Operations

### Restart behavior

When `restart` is sent as a DM (owner only), the bot sends an acknowledgement, marks the restart as in-progress, and replaces itself via `syscall.Exec`. The Zulip event queue is **not deregistered** so the new process can resume it. On startup, if `restart_startup_notification = true`, the new process sends a "restart complete" DM to the original requester.

### BAD_EVENT_QUEUE_ID recovery

If the Zulip server discards the bot's event queue, the loop registers a new queue and continues. An audit record (`event_queue.recover`) is written.

### Queue persistence and resume

On startup the bot tries to resume the stored queue. If the queue is no longer valid, it registers a new one.

### Reaction handling against bad mappings

Reactions on the announcement message are dispatched through the enabled emoji→group mappings. Three explicit outcomes:

- **ignored** — reaction is not on the announcement, is the bot's own reaction, the emoji has no enabled mapping, or no announcement message is configured. State advances; no record is written.
- **processed** — `SubscribeUser`/`UnsubscribeUser` succeeded. The reaction is recorded in `processed_reactions` so retries are no-ops, and the queue advances.
- **failed** — the mapping exists but the operation failed for a domain/configuration reason. Today the only failure classified as "failed" is a missing channel group (`channelgroup.ErrChannelGroupNotFound`). The bot logs the failure once, records the reaction in `processed_reactions` (so it is **not** retried from the Zulip event queue), and advances `last_event_id`. This prevents the rate-limit hot-loop that bad data previously caused.

If you spot recurring "channel group X not found" log entries, fix the offending mapping (`group mapping set` with a valid `zulip_user_group_id`, or `group mapping disable` to remove it) — the bot auto-imports the user group locally as part of `mapping set` if needed. Existing reactions that were classified as failed will not retry automatically — affected users must remove and re-add their reaction (a dedicated replay command can be added later if needed).

True infrastructure failures (database errors, Zulip API outages) still log at ERROR, but the queue advances so the loop cannot get stuck retrying the same event forever.

## Project Layout

```
cmd/campusbot/                     entry point
internal/
  callorigin/                      context-tagging for test doubles
  channelgroup/
    api_channel_groups.go          channel group API (subscribe/unsubscribe)
    service.go                     GroupService: user-oriented subscribe/unsubscribe adapter
    db/                            sqlc-generated channelgroup queries
  zulipmock/                       in-memory Zulip client mock for tests
  zulipbot/
    announcement/
      renderer.go                  renders the announcement markdown
      manager.go                   manages send/edit lifecycle and bot reactions
    audit/                         audit record types
    command/                       command parsing, registry, router, help
    configsvc/                     bot configuration service
    handlers/
      config.go                    config command handler
      group.go                     group command handler (subscribe/unsubscribe/mapping/announce)
      restart.go                   restart command handler
      status.go                    status command handler
    storage/
      storage.go                   repository wrapper
      sql/                         schema and query SQL files
      db/                          sqlc-generated code
    app.go                         application wiring
    bot.go                         Zulip client wrapper
    loop.go                        event polling loop, reaction handling
    source.go                      Zulip event queue source
```
