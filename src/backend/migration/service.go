package migration

import (
	"context"
	"errors"
	"strings"
	"time"

	"wbd/backend/database"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidSQL        = errors.New("migration SQL is empty")
	ErrInvalidVersion    = errors.New("invalid migration version")
	ErrMigrationNotFound = errors.New("migration not found")
	ErrMigrationLocked   = errors.New("migration cannot be changed")
	ErrMigrationFailed   = errors.New("migration execution failed")
)

type Service struct {
	pools database.TargetPoolProvider
}

func NewService(pools database.TargetPoolProvider) *Service {
	return &Service{pools: pools}
}

func (s *Service) List(ctx context.Context, databaseID database.DatabaseID) ([]Migration, error) {
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	if err := ensureTable(ctx, pool); err != nil {
		return nil, database.ErrDatabaseUnavailable
	}
	rows, err := pool.Query(ctx, migrationSelect+` ORDER BY version`)
	if err != nil {
		return nil, database.ErrDatabaseUnavailable
	}
	defer rows.Close()
	result := make([]Migration, 0)
	for rows.Next() {
		item, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Service) Get(ctx context.Context, databaseID database.DatabaseID, version int64) (Migration, error) {
	if version <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return Migration{}, err
	}
	if err := ensureTable(ctx, pool); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	item, err := scanMigration(pool.QueryRow(ctx, migrationSelect+` WHERE version = $1`, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, ErrMigrationNotFound
	}
	return item, err
}

func (s *Service) Create(ctx context.Context, databaseID database.DatabaseID, userID int64, sqlText string) (Migration, error) {
	if err := validateSQL(sqlText); err != nil {
		return Migration{}, err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return Migration{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	defer tx.Rollback(ctx)
	if err := ensureTable(ctx, tx); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := lockMigrations(ctx, tx); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM platform_migrations`).Scan(&version); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	item, err := insertMigration(ctx, tx, version, sqlText, userID, "draft")
	if err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	return item, nil
}

func (s *Service) Update(ctx context.Context, databaseID database.DatabaseID, version int64, sqlText string) (Migration, error) {
	if version <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	if err := validateSQL(sqlText); err != nil {
		return Migration{}, err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return Migration{}, err
	}
	if err := ensureTable(ctx, pool); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	var item Migration
	item, err = scanMigration(pool.QueryRow(ctx, migrationSelect+` WHERE version = $1`, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if item.Status != "draft" {
		return Migration{}, ErrMigrationLocked
	}
	err = pool.QueryRow(ctx, `
		UPDATE platform_migrations
		SET sql_text = $2
		WHERE version = $1 AND status = 'draft'
		RETURNING `+migrationColumns, version, sqlText).Scan(migrationScanArgs(&item)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, ErrMigrationLocked
	}
	if err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	return item, nil
}

func (s *Service) Execute(ctx context.Context, databaseID database.DatabaseID, version, userID int64) (Migration, error) {
	if version <= 0 || userID <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return Migration{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := ensureTable(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := lockMigrations(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return Migration{}, database.ErrDatabaseUnavailable
	}
	var item Migration
	item, err = scanMigration(tx.QueryRow(ctx, migrationSelect+` WHERE version = $1 FOR UPDATE`, version))
	if errors.Is(err, pgx.ErrNoRows) {
		tx.Rollback(ctx)
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		tx.Rollback(ctx)
		return Migration{}, err
	}
	if item.Status != "draft" {
		tx.Rollback(ctx)
		return Migration{}, ErrMigrationLocked
	}
	if _, err := tx.Exec(ctx, `UPDATE platform_migrations SET status = 'running', executed_by = $2 WHERE version = $1`, version, userID); err != nil {
		tx.Rollback(ctx)
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if _, err := tx.Exec(ctx, item.SQL); err != nil {
		tx.Rollback(ctx)
		if markFailedErr := markFailed(ctx, pool, version, userID, err.Error()); markFailedErr != nil {
			return Migration{}, markFailedErr
		}
		return Migration{}, ErrMigrationFailed
	}
	if _, err := tx.Exec(ctx, `
		UPDATE platform_migrations
		SET status = 'success', executed_by = $2, executed_at = NOW(), error_message = NULL
		WHERE version = $1`, version, userID); err != nil {
		tx.Rollback(ctx)
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	item.Status = "success"
	item.ExecutedBy = &userID
	now := time.Now()
	item.ExecutedAt = &now
	item.ErrorMessage = ""
	return item, nil
}

const migrationColumns = `version, sql_text, created_by, executed_by, created_at, executed_at, status, error_message`
const migrationSelect = `SELECT ` + migrationColumns + ` FROM platform_migrations`

type rowScanner interface {
	Scan(...any) error
}

func scanMigration(row rowScanner, optional ...*Migration) (Migration, error) {
	var item Migration
	err := row.Scan(&item.Version, &item.SQL, &item.CreatedBy, &item.ExecutedBy, &item.CreatedAt, &item.ExecutedAt, &item.Status, &item.ErrorMessage)
	if len(optional) > 0 {
		*optional[0] = item
	}
	return item, err
}

func migrationScanArgs(item *Migration) []any {
	return []any{&item.Version, &item.SQL, &item.CreatedBy, &item.ExecutedBy, &item.CreatedAt, &item.ExecutedAt, &item.Status, &item.ErrorMessage}
}

func insertMigration(ctx context.Context, tx pgx.Tx, version int64, sqlText string, userID int64, status string) (Migration, error) {
	return scanMigration(tx.QueryRow(ctx, `
		INSERT INTO platform_migrations (version, sql_text, created_by, status)
		VALUES ($1, $2, $3, $4)
		RETURNING `+migrationColumns, version, sqlText, userID, status))
}

func ensureTable(ctx context.Context, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) error {
	_, err := executor.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS platform_migrations (
			version BIGINT PRIMARY KEY,
			sql_text TEXT NOT NULL,
			created_by BIGINT NOT NULL,
			executed_by BIGINT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			executed_at TIMESTAMPTZ,
			status TEXT NOT NULL CHECK (status IN ('draft', 'running', 'success', 'failed')),
			error_message TEXT
		)`)
	return err
}

func lockMigrations(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('platform_migrations', 0))`)
	return err
}

func markFailed(ctx context.Context, pool *pgxpool.Pool, version, userID int64, message string) error {
	_, err := pool.Exec(ctx, `
		UPDATE platform_migrations
		SET status = 'failed', executed_by = $2, executed_at = NOW(), error_message = $3
		WHERE version = $1`, version, userID, message)
	return err
}

func validateSQL(sqlText string) error {
	if strings.TrimSpace(sqlText) == "" {
		return ErrInvalidSQL
	}
	return nil
}
