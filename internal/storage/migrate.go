package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/kutonlagos/agentmux/migrations"
)

// schemaMigrationsTable records which migrations have run.
//
// It is created outside the migration system on purpose: the table that tracks
// migrations cannot itself be a migration.
const schemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER NOT NULL PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at TEXT    NOT NULL
);`

// MigrationResult reports what a Migrate call did.
type MigrationResult struct {
	// Applied lists the migrations executed by this call, in order.
	Applied []string

	// Version is the schema version after the call.
	Version int
}

// Migrate brings the database up to the latest schema version.
//
// Each migration runs inside its own transaction together with the record that
// marks it applied, so an interrupted migration is either fully applied or not
// applied at all. Re-running Migrate is a no-op.
func (s *Store) Migrate(ctx context.Context) (MigrationResult, error) {
	all, err := migrations.All()
	if err != nil {
		return MigrationResult{}, err
	}
	if _, err := s.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		return MigrationResult{}, fmt.Errorf("storage: create schema_migrations: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return MigrationResult{}, err
	}

	var result MigrationResult
	for _, migration := range all {
		if applied[migration.Version] {
			result.Version = migration.Version
			continue
		}
		if err := s.applyMigration(ctx, migration); err != nil {
			return result, err
		}
		result.Applied = append(result.Applied, migration.String())
		result.Version = migration.Version
	}
	return result, nil
}

// SchemaVersion reports the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx, schemaMigrationsTable); err != nil {
		return 0, fmt.Errorf("storage: create schema_migrations: %w", err)
	}
	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return 0, err
	}
	version := 0
	for v := range applied {
		if v > version {
			version = v
		}
	}
	return version, nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("storage: scan schema_migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("storage: read schema_migrations: %w", err)
	}
	return applied, nil
}

func (s *Store) applyMigration(ctx context.Context, migration migrations.Migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("storage: begin migration %s: %w", migration, err)
	}
	// Rollback is a no-op once the transaction has committed.
	defer func() { _ = tx.Rollback() }()

	// The driver executes a multi-statement body as a batch, so a migration
	// file does not need to be split by hand.
	if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("storage: apply migration %s: %w", migration, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		migration.Version, migration.Name, formatTime(time.Now()),
	); err != nil {
		return fmt.Errorf("storage: record migration %s: %w", migration, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit migration %s: %w", migration, err)
	}
	return nil
}
