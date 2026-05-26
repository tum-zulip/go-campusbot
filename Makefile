BINARY := bin/campusbot
CMD := ./cmd/campusbot
GENERATE_PKG := ./internal

SQL_FILES := \
	internal/zulipbot/storage/sql/schema.sql \
	internal/zulipbot/storage/sql/queries.sql

SQLC_CONFIG := sqlc.yaml

GENERATED := \
	internal/zulipbot/storage/db/db.go \
	internal/zulipbot/storage/db/models.go \
	internal/zulipbot/storage/db/queries.sql.go

.PHONY: build generate lint debug clean

.DEFAULT_GOAL := build

# sqlc writes all GENERATED files together; db.go triggers regen.
internal/zulipbot/storage/db/db.go: $(SQL_FILES) $(SQLC_CONFIG) internal/generate.go
	go generate $(GENERATE_PKG)

$(filter-out internal/zulipbot/storage/db/db.go,$(GENERATED)): internal/zulipbot/storage/db/db.go ;

generate: $(GENERATED)

build: $(GENERATED)
	@mkdir -p bin
	go build -o $(BINARY) $(CMD)

lint: $(GENERATED)
	golangci-lint run ./...

debug: build
	$(BINARY) --log-level debug $(ARGS)

clean:
	rm -rf bin
	rm -f $(GENERATED)
