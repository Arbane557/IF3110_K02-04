package db

import (
	"context"
	"database/sql"
	_ "embed"
)

//go:embed init/005_migration_jobs.sql
var migrationJobs string

// InstallMigrationJobs upgrades both fresh and existing platform databases.
func InstallMigrationJobs(ctx context.Context, database *sql.DB) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('wbd_platform_schema', 0))"); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, migrationJobs); err != nil {
		return err
	}
	return tx.Commit()
}
