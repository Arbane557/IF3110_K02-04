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
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidSQL        = errors.New("migration SQL is empty")
	ErrInvalidVersion    = errors.New("invalid migration version")
	ErrMigrationNotFound = errors.New("migration not found")
	ErrMigrationLocked   = errors.New("migration cannot be changed")
	ErrMigrationFailed   = errors.New("migration execution failed")
	ErrMigrationStore    = errors.New("migration store unavailable")
)

type Service struct {
	pools      database.TargetPoolProvider
	internalDB *sql.DB
	observer   observability.Observer
}

func NewService(internalDB *sql.DB, pools database.TargetPoolProvider, observers ...observability.Observer) *Service {
	service := &Service{internalDB: internalDB, pools: pools}
	if len(observers) > 0 {
		service.observer = observers[0]
	}
	return service
}

func (s *Service) List(ctx context.Context, databaseID database.DatabaseID) ([]Migration, error) {
	if s.internalDB == nil {
		return nil, ErrMigrationStore
	}
	rows, err := s.internalDB.QueryContext(ctx, migrationSelect+` WHERE database_id = $1 ORDER BY version`, databaseID)
	if err != nil {
		return nil, ErrMigrationStore
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
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	item, err := scanMigration(s.internalDB.QueryRowContext(ctx,
		migrationSelect+` WHERE database_id = $1 AND version = $2`, databaseID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		return Migration{}, ErrMigrationStore
	}
	return item, err
}

func (s *Service) Create(ctx context.Context, databaseID database.DatabaseID, userID int64, sqlText string) (Migration, error) {
	if err := validateSQL(sqlText); err != nil {
		return Migration{}, err
	}
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, ErrMigrationStore
	}
	defer tx.Rollback()
	if err := lockInternalMigrations(ctx, tx, databaseID); err != nil {
		return Migration{}, ErrMigrationStore
	}
	var version int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) + 1 FROM migrations WHERE database_id = $1`,
		databaseID).Scan(&version); err != nil {
		return Migration{}, ErrMigrationStore
	}
	item, err := scanMigration(tx.QueryRowContext(ctx, `
		INSERT INTO migrations (database_id, version, sql_text, created_by, status)
		VALUES ($1, $2, $3, $4, 'draft')
		RETURNING `+migrationColumns, databaseID, version, sqlText, userID))
	if err != nil {
		return Migration{}, ErrMigrationStore
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, ErrMigrationStore
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
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	var item Migration
	item, err := scanMigration(s.internalDB.QueryRowContext(ctx,
		migrationSelect+` WHERE database_id = $1 AND version = $2`, databaseID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if item.Status != "draft" {
		return Migration{}, ErrMigrationLocked
	}
	item, err = scanMigration(s.internalDB.QueryRowContext(ctx, `
		UPDATE migrations
		SET sql_text = $3
		WHERE database_id = $1 AND version = $2 AND status = 'draft'
		RETURNING `+migrationColumns, databaseID, version, sqlText))
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrMigrationLocked
	}
	if err != nil {
		return Migration{}, ErrMigrationStore
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
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	item, err := s.startExecution(ctx, databaseID, version, userID)
	if err != nil {
		return Migration{}, err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	s.recordPoolStats(pool)
	defer s.recordPoolStats(pool)
	if err := lockMigrations(ctx, tx); err != nil {
		tx.Rollback(ctx)
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
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
		if markFailedErr := s.markFailed(ctx, databaseID, version, userID, execErr.Error()); markFailedErr != nil {
			return Migration{}, markFailedErr
		}
		return Migration{}, ErrMigrationFailed
	}
	if err := tx.Commit(ctx); err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := s.markSuccess(ctx, databaseID, version, userID); err != nil {
		return Migration{}, err
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
const migrationSelect = `SELECT ` + migrationColumns + ` FROM migrations`

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

func lockMigrations(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('migration_execution', 0))`)
	return err
}

func lockInternalMigrations(ctx context.Context, tx *sql.Tx, databaseID database.DatabaseID) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, databaseID)
	return err
}

func (s *Service) startExecution(
	ctx context.Context,
	databaseID database.DatabaseID,
	version, userID int64,
) (Migration, error) {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, ErrMigrationStore
	}
	defer tx.Rollback()
	if err := lockInternalMigrations(ctx, tx, databaseID); err != nil {
		return Migration{}, ErrMigrationStore
	}

	item, err := scanMigration(tx.QueryRowContext(ctx,
		migrationSelect+` WHERE database_id = $1 AND version = $2 FOR UPDATE`, databaseID, version))
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrMigrationNotFound
	}
	if err != nil {
		return Migration{}, ErrMigrationStore
	}
	if item.Status != "draft" {
		return Migration{}, ErrMigrationLocked
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'running', executed_by = $3, error_message = NULL
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID); err != nil {
		return Migration{}, ErrMigrationStore
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, ErrMigrationStore
	}
	item.Status = "running"
	item.ExecutedBy = &userID
	return item, nil
}

func (s *Service) markSuccess(
	ctx context.Context,
	databaseID database.DatabaseID,
	version, userID int64,
) error {
	_, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'success', executed_by = $3, executed_at = NOW(), error_message = NULL
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID)
	if err != nil {
		return ErrMigrationStore
	}
	return nil
}

func (s *Service) markFailed(
	ctx context.Context,
	databaseID database.DatabaseID,
	version, userID int64,
	message string,
) error {
	_, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'failed', executed_by = $3, executed_at = NOW(), error_message = $4
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID, message)
	if err != nil {
		return ErrMigrationStore
	}
	return nil
}

func validateSQL(sqlText string) error {
	if strings.TrimSpace(sqlText) == "" {
		return ErrInvalidSQL
	}
	return nil
}
