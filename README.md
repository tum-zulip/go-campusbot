# go-campusbot

Go Zulip bot for campus automation. It listens to Zulip events, handles bot
commands, stores state in SQLite, and can use additional worker bot credentials
for background Zulip requests.

## Requirements

- Go 1.27.1+
- A C compiler (SQLite uses CGO)
- GNU Make 4.3+ for Makefile targets (`gmake` on macOS)
- A Zulip bot `zuliprc` file

## Run Locally

```sh
go generate ./internal
go run ./cmd/campusbot --zuliprc path/to/zuliprc --db campusbot.sqlite3
```

Useful options:

- `--worker-rc-dir`: directory of worker `zuliprc` files
- `--log-level`: `verbose`, `debug`, `info`, `warn`, or `error`
- `--log-format`: `text` or `json`

## Deploy

Use the published GitHub releases for deployment binaries. See `INSTALL` for a
minimal systemd setup.

## Development

```sh
go generate ./internal
go test ./...
```
