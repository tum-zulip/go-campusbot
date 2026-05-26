package sqlitemigrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CompatibilityStep performs package-specific repair work for schemas created
// before the package had complete migration coverage.
type CompatibilityStep func(context.Context, *sql.DB) error

type Config struct {
	PackageName        string
	DB                 *sql.DB
	SchemaSQL          string
	MigrationTable     string
	CurrentVersion     int
	BaselineName       string
	CompatibilitySteps []CompatibilityStep
}

func Apply(ctx context.Context, cfg Config) error {
	if cfg.DB == nil {
		return fmt.Errorf("%s database connection must not be nil", packageName(cfg.PackageName))
	}
	if cfg.SchemaSQL == "" {
		return fmt.Errorf("%s schema SQL must not be empty", packageName(cfg.PackageName))
	}
	if cfg.MigrationTable == "" {
		return fmt.Errorf("%s migration table must not be empty", packageName(cfg.PackageName))
	}
	if cfg.CurrentVersion <= 0 {
		return fmt.Errorf("%s current schema version must be positive", packageName(cfg.PackageName))
	}
	if cfg.BaselineName == "" {
		return fmt.Errorf("%s schema baseline name must not be empty", packageName(cfg.PackageName))
	}

	if _, err := cfg.DB.ExecContext(ctx, cfg.SchemaSQL); err != nil {
		return fmt.Errorf("apply %s SQLite schema: %w", packageName(cfg.PackageName), err)
	}
	for _, step := range cfg.CompatibilitySteps {
		if step == nil {
			continue
		}
		if err := step(ctx, cfg.DB); err != nil {
			return err
		}
	}

	table := quoteIdentifier(cfg.MigrationTable)
	var version int
	if err := cfg.DB.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+table).Scan(&version); err != nil {
		return fmt.Errorf("read %s schema version: %w", packageName(cfg.PackageName), err)
	}
	if version != cfg.CurrentVersion {
		return fmt.Errorf(
			"%s database schema version %d does not match development schema version %d; reset the database or add a migration policy",
			packageName(cfg.PackageName),
			version,
			cfg.CurrentVersion,
		)
	}

	var name string
	if err := cfg.DB.QueryRowContext(ctx, "SELECT name FROM "+table+" WHERE version = ?", cfg.CurrentVersion).Scan(&name); err != nil {
		return fmt.Errorf("read %s schema baseline name: %w", packageName(cfg.PackageName), err)
	}
	if name != cfg.BaselineName {
		return fmt.Errorf(
			"%s database schema baseline %q is not compatible with the current development schema %q; reset the database or add a migration policy",
			packageName(cfg.PackageName),
			name,
			cfg.BaselineName,
		)
	}
	return nil
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func packageName(name string) string {
	if name == "" {
		return "SQLite"
	}
	return name
}
