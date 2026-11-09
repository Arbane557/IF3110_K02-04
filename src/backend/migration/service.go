package migration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"wbd/backend/database"
	"wbd/backend/observability"

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
	pools    database.TargetPoolProvider
	observer observability.Observer
}

func NewService(pools database.TargetPoolProvider, observers ...observability.Observer) *Service {
	service := &Service{pools: pools}
	if len(observers) > 0 {
		service.observer = observers[0]
	}
	return service
}

func (s *Service) List(ctx context.Context, databaseID database.DatabaseID) ([]Migration, error) {
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, migrationSelect+` ORDER BY version`)
	if err != nil {
		if isMissingMigrationTable(err) {
			return []Migration{}, nil
		}
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
	item, err := scanMigration(pool.QueryRow(ctx, migrationSelect+` WHERE version = $1`, version))
	if errors.Is(err, pgx.ErrNoRows) || isMissingMigrationTable(err) {
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
	var item Migration
	item, err = scanMigration(pool.QueryRow(ctx, migrationSelect+` WHERE version = $1`, version))
	if errors.Is(err, pgx.ErrNoRows) || isMissingMigrationTable(err) {
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if item.Status != "draft" {
		return Migration{}, ErrMigrationLocked
	}
	item, err = scanMigration(pool.QueryRow(ctx, `
		UPDATE platform_migrations
		SET sql_text = $2
		WHERE version = $1 AND status = 'draft'
		RETURNING `+migrationColumns, version, sqlText))
	if errors.Is(err, pgx.ErrNoRows) {
		return Migration{}, ErrMigrationLocked
	}
	if err != nil {
		return Migration{}, database.ErrDatabaseUnavailable
	}
	return item, nil
}

func (s *Service) Explain(ctx context.Context, databaseID database.DatabaseID, sqlText string) (string, error) {
	if err := validateSQL(sqlText); err != nil {
		return "", err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return "", err
	}
	rows, err := pool.Query(ctx, "EXPLAIN "+sqlText)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) Execute(ctx context.Context, databaseID database.DatabaseID, version, userID int64) (result Migration, err error) {
	startedAt := time.Now()
	defer func() {
		if s.observer == nil {
			return
		}
		status := "success"
		if err != nil {
			status = "failed"
		}
		s.observer.RecordMigration(observability.MigrationResult{
			Direction: "up",
			Status:    status,
			Duration:  time.Since(startedAt),
		})
	}()

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
	s.recordPoolStats(pool)
	defer s.recordPoolStats(pool)
	if err := lockMigrations(ctx, tx); err != nil {
		tx.Rollback(ctx)
		return Migration{}, database.ErrDatabaseUnavailable
	}
	var item Migration
	item, err = scanMigration(tx.QueryRow(ctx, migrationSelect+` WHERE version = $1 FOR UPDATE`, version))
	if errors.Is(err, pgx.ErrNoRows) || isMissingMigrationTable(err) {
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
	batchStartedAt := time.Now()
	commandTag, execErr := tx.Exec(ctx, item.SQL)
	if s.observer != nil {
		batchStatus := "success"
		if execErr != nil {
			batchStatus = "failed"
		}
		rowsAffected := int64(0)
		if execErr == nil {
			rowsAffected = commandTag.RowsAffected()
		}
		s.observer.RecordBatch(observability.BatchResult{
			Status:       batchStatus,
			Duration:     time.Since(batchStartedAt),
			RowsAffected: rowsAffected,
		})
	}
	if execErr != nil {
		tx.Rollback(ctx)
		if markFailedErr := markFailed(ctx, pool, version, userID, execErr.Error()); markFailedErr != nil {
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

func (s *Service) recordPoolStats(pool *pgxpool.Pool) {
	if s.observer == nil {
		return
	}
	stats := pool.Stat()
	s.observer.SetDatabasePool(stats.AcquiredConns(), stats.IdleConns())
}

const migrationColumns = `version, sql_text, created_by, executed_by, created_at, executed_at, status, error_message`
const migrationSelect = `SELECT ` + migrationColumns + ` FROM platform_migrations`

type rowScanner interface {
	Scan(...any) error
}

func scanMigration(row rowScanner, optional ...*Migration) (Migration, error) {
	var item Migration
	var errorMessage sql.NullString
	err := row.Scan(&item.Version, &item.SQL, &item.CreatedBy, &item.ExecutedBy, &item.CreatedAt, &item.ExecutedAt, &item.Status, &errorMessage)
	if err == nil && errorMessage.Valid {
		item.ErrorMessage = errorMessage.String
	}
	if len(optional) > 0 {
		*optional[0] = item
	}
	return item, err
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

func isMissingMigrationTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
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
