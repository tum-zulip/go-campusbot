.PHONY: sqlc
sqlc:
	go tool github.com/sqlc-dev/sqlc/cmd/sqlc generate
