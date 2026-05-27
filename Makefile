BINARY := bin/campusbot
CMD := ./cmd/campusbot
GENERATE_PKG := ./internal

SQL_FILES := \
	internal/zulipbot/storage/db/sql/schema.sql \
	internal/zulipbot/storage/db/sql/queries.sql \
	internal/channelgroup/db/sql/schema.sql \
	internal/channelgroup/db/sql/query.sql

SQLC_CONFIG := sqlc.yaml

GENERATED := \
	internal/zulipbot/storage/db/db.go \
	internal/zulipbot/storage/db/models.go \
	internal/zulipbot/storage/db/queries.sql.go \
	internal/channelgroup/db/db.go \
	internal/channelgroup/db/models.go \
	internal/channelgroup/db/querier.go \
	internal/channelgroup/db/query.sql.go

.PHONY: build generate lint debug verbose clean

.DEFAULT_GOAL := build

# sqlc writes all GENERATED files together from the root sqlc.yaml.
$(GENERATED) &: $(SQL_FILES) $(SQLC_CONFIG) internal/generate.go
	go generate $(GENERATE_PKG)

generate: $(GENERATED)

build: $(GENERATED)
	@mkdir -p bin
	go build -o $(BINARY) $(CMD)

lint: $(GENERATED)
	golangci-lint run ./...

debug: build
	$(BINARY) --log-level debug $(ARGS)

verbose: build
	$(BINARY) --log-level verbose $(ARGS)

clean:
	rm -rf bin
	rm -f $(GENERATED)
