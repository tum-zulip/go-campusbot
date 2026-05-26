package channelgroup

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	"github.com/tum-zulip/go-campusbot/internal/sqlitemigrate"
)

const (
	currentSchemaVersion = 1
	schemaBaselineName   = "channelgroup baseline"
)

//go:embed db/schema.sql
var schemaSQL string

// Migrate applies and validates the channelgroup-owned SQLite schema.
func Migrate(ctx context.Context, db *sql.DB) error {
	return sqlitemigrate.Apply(ctx, sqlitemigrate.Config{
		PackageName:        "channelgroup",
		DB:                 db,
		SchemaSQL:          schemaSQL,
		MigrationTable:     "channelgroup_schema_migrations",
		CurrentVersion:     currentSchemaVersion,
		BaselineName:       schemaBaselineName,
		CompatibilitySteps: []sqlitemigrate.CompatibilityStep{ensureChannelGroupChannelFolderColumn},
	})
}

func ensureChannelGroupChannelFolderColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(channel_groups)")
	if err != nil {
		return fmt.Errorf("inspect channel_groups schema: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return fmt.Errorf("scan channel_groups schema: %w", err)
		}
		if name == "channel_folder_id" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate channel_groups schema: %w", err)
	}

	if _, err := db.ExecContext(ctx, "ALTER TABLE channel_groups ADD COLUMN channel_folder_id INTEGER"); err != nil {
		return fmt.Errorf("add channel_groups.channel_folder_id column: %w", err)
	}
	return nil
}
