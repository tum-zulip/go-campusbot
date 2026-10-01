package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "embed"

	// Import the SQLite driver for callers that initialize storage through this package.
	_ "github.com/mattn/go-sqlite3"
)

// FormatTime renders a timestamp in the canonical format used for all
// timestamp columns in this storage schema.
func FormatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

//go:embed sql/schema.sql
var schemaSQL string

func ConfigureSQLite(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database connection must not be nil")
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable SQLite foreign keys: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return fmt.Errorf("enable SQLite WAL journal mode: %w", err)
	}
	return nil
}

func InitSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("database connection must not be nil")
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("initialize zulipbot storage schema: %w", err)
	}
	return nil
}
