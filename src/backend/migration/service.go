package migration

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
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
	ErrMigrationStore    = errors.New("migration store unavailable")
	ErrIrreversible      = errors.New("migration is irreversible")
	ErrNotLatestActive   = errors.New("only the latest active migration can be rolled back")
	ErrForwardNotNext    = errors.New("migration is not the next forward migration")
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
	return s.CreateWithDown(ctx, databaseID, userID, sqlText, "", true, "")
}

func (s *Service) CreateWithDown(ctx context.Context, databaseID database.DatabaseID, userID int64, sqlText, sqlDown string, irreversible bool, irreversibleReason string) (Migration, error) {
	if err := validateSQL(sqlText); err != nil {
		return Migration{}, err
	}
	if !irreversible && strings.TrimSpace(sqlDown) == "" {
		return Migration{}, errors.New("reversible migration requires SQL down")
	}
	if irreversible && strings.TrimSpace(irreversibleReason) == "" {
		irreversibleReason = "migration is irreversible"
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
		INSERT INTO migrations (database_id, version, sql_text, sql_down, reversible, irreversible_reason, active, created_by, status)
		VALUES ($1, $2, $3, $4, $5, $6, FALSE, $7, 'draft')
		RETURNING `+migrationColumns, databaseID, version, sqlText, sqlDown, !irreversible, nullableString(irreversibleReason), userID))
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
        AND NOT EXISTS (SELECT 1 FROM migration_jobs j WHERE j.database_id=$1 AND j.version=$2
            AND (j.blocks_database OR j.status IN ('pending','running','pending_review')))
        AND NOT EXISTS (SELECT 1 FROM migration_history h WHERE h.database_id=$1 AND h.version=$2)
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
	statements, err := splitMigrationSQL(sqlText)
	if err != nil || len(statements) != 1 {
		return "", errors.New("preview requires exactly one SQL statement")
	}
	keyword := statementStart.FindString(statements[0].log)
	switch strings.ToUpper(keyword) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "WITH", "VALUES", "TABLE":
	default:
		return "", errors.New("statement is not supported by preview")
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		return "", err
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, "EXPLAIN "+statements[0].sql, pgx.QueryExecModeDescribeExec)
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
	var historyID int64

	// Full migration
	ctx, executionSpan := s.startTrace(ctx, observability.SpanMigrationExecute, observability.Fields{
		"database.id":         int64(databaseID),
		"migration.version":   version,
		"migration.direction": "up",
	})
	defer func() {
		status := "success"
		level := slog.LevelInfo
		if err != nil {
			status = "failed"
			level = slog.LevelError
		}
		fields := observability.Fields{
			"actor":               userID,
			"database":            int64(databaseID),
			"status":              status,
			"migration.version":   version,
			"migration.direction": "up",
			"duration_ms":         time.Since(startedAt).Milliseconds(),
		}
		if historyID != 0 {
			if err != nil {
				_ = s.markHistory(ctx, historyID, "failed", err.Error())
			} else {
				_ = s.markHistory(ctx, historyID, "success", "")
			}
		}
		if err != nil {
			fields["error"] = err.Error()
			if state := sqlState(err); state != "" {
				fields["sqlstate"] = state
			}
		}
		s.log(ctx, level, observability.EventMigrationFinished, fields)
		endTrace(executionSpan, err)

		if s.observer == nil {
			return
		}
		s.observer.RecordMigration(observability.MigrationResult{
			Direction: "up",
			Status:    status,
			Duration:  time.Since(startedAt),
		})
	}()
	s.log(ctx, slog.LevelInfo, observability.EventMigrationStarted, observability.Fields{
		"actor":               userID,
		"database":            int64(databaseID),
		"status":              "running",
		"migration.version":   version,
		"migration.direction": "up",
	})

	if version <= 0 || userID <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}

	// Start record
	startContext, startSpan := s.startTrace(ctx, observability.SpanMigrationStartExecution, observability.Fields{
		"database.id":       int64(databaseID),
		"migration.version": version,
	})
	item, err := s.startExecution(startContext, databaseID, version, userID)
	endTrace(startSpan, err)
	if err != nil {
		return Migration{}, err
	}
	historyID, err = s.startHistory(ctx, databaseID, version, userID, "up", item.SQL, nil)
	if err != nil {
		return Migration{}, err
	}

	// Get target DB
	acquireContext, acquireSpan := s.startTrace(ctx, observability.SpanMigrationAcquireTarget, observability.Fields{
		"database.id": int64(databaseID),
	})
	pool, err := s.pools.Pool(acquireContext, databaseID)
	endTrace(acquireSpan, err)
	if err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, err
	}

	// Begin transaction
	beginContext, beginSpan := s.startTrace(ctx, observability.SpanMigrationBeginTarget, observability.Fields{
		"database.id": int64(databaseID),
	})
	tx, err := pool.Begin(beginContext)
	endTrace(beginSpan, err)
	if err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	s.recordPoolStats(pool)
	defer s.recordPoolStats(pool)

	// Lock migration
	lockContext, lockSpan := s.startTrace(ctx, observability.SpanMigrationLockTarget, observability.Fields{
		"database.id": int64(databaseID),
	})
	err = lockMigrations(lockContext, tx)
	endTrace(lockSpan, err)
	if err != nil {
		s.rollbackTarget(ctx, tx, databaseID, version, userID)
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}

	// Run SQL batch
	batchStartedAt := time.Now()
	batchContext, batchSpan := s.startTrace(ctx, observability.SpanMigrationBatch, observability.Fields{
		"database.id":       int64(databaseID),
		"migration.version": version,
		"batch.number":      1,
	})
	s.log(batchContext, slog.LevelInfo, observability.EventBatchStarted, observability.Fields{
		"actor":             userID,
		"database":          int64(databaseID),
		"status":            "running",
		"migration.version": version,
		"attempt":           1,
		"batch":             1,
		"query":             item.SQL,
		"parameters":        []any{},
	})
	commandTag, execErr := tx.Exec(batchContext, item.SQL)
	batchStatus := "success"
	rowsAffected := int64(0)
	batchLevel := slog.LevelInfo
	if execErr != nil {
		batchStatus = "failed"
		batchLevel = slog.LevelError
	} else {
		rowsAffected = commandTag.RowsAffected()
	}
	batchFields := observability.Fields{
		"actor":             userID,
		"database":          int64(databaseID),
		"status":            batchStatus,
		"migration.version": version,
		"attempt":           1,
		"batch":             1,
		"query":             item.SQL,
		"parameters":        []any{},
		"duration_ms":       time.Since(batchStartedAt).Milliseconds(),
		"rows":              rowsAffected,
	}
	if execErr != nil {
		batchFields["error"] = execErr.Error()
		if state := sqlState(execErr); state != "" {
			batchFields["sqlstate"] = state
		}
	}
	s.log(batchContext, batchLevel, observability.EventBatchFinished, batchFields)
	if execErr == nil && batchSpan != nil {
		batchSpan.AddEvent(observability.EventMigrationBatchResult, observability.Fields{
			"batch.rows_affected": commandTag.RowsAffected(),
		})
	}
	endTrace(batchSpan, execErr)
	if s.observer != nil {
		s.observer.RecordBatch(observability.BatchResult{
			Status:       batchStatus,
			Duration:     time.Since(batchStartedAt),
			RowsAffected: rowsAffected,
		})
	}
	if execErr != nil {
		s.rollbackTarget(ctx, tx, databaseID, version, userID)
		if markFailedErr := s.markFailed(ctx, databaseID, version, userID, execErr.Error()); markFailedErr != nil {
			return Migration{}, markFailedErr
		}
		return Migration{}, ErrMigrationFailed
	}

	// Commit transaction
	commitContext, commitSpan := s.startTrace(ctx, observability.SpanMigrationCommitTarget, observability.Fields{
		"database.id": int64(databaseID),
	})
	commitStartedAt := time.Now()
	err = tx.Commit(commitContext)
	commitStatus := "success"
	commitLevel := slog.LevelInfo
	commitFields := observability.Fields{
		"actor":             userID,
		"database":          int64(databaseID),
		"migration.version": version,
		"duration_ms":       time.Since(commitStartedAt).Milliseconds(),
	}
	if err != nil {
		commitStatus = "failed"
		commitLevel = slog.LevelError
		commitFields["error"] = err.Error()
		if state := sqlState(err); state != "" {
			commitFields["sqlstate"] = state
		}
	}
	commitFields["status"] = commitStatus
	s.log(commitContext, commitLevel, observability.EventCommit, commitFields)
	endTrace(commitSpan, err)
	if err != nil {
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}

	// Finish record
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

func (s *Service) Rollback(ctx context.Context, databaseID database.DatabaseID, version, userID int64) (Migration, error) {
	if version <= 0 || userID <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	item, err := s.Get(ctx, databaseID, version)
	if err != nil {
		return Migration{}, err
	}
	if !item.Reversible || strings.TrimSpace(item.SQLDown) == "" {
		return Migration{}, ErrIrreversible
	}
	if !item.Active {
		return Migration{}, ErrMigrationLocked
	}
	var latest int64
	if err := s.internalDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM migrations WHERE database_id = $1 AND active = TRUE`,
		databaseID).Scan(&latest); err != nil {
		return Migration{}, ErrMigrationStore
	}
	if latest != version {
		return Migration{}, ErrNotLatestActive
	}

	cancelledExecutionID, err := s.latestUncancelledExecution(ctx, databaseID, version)
	if err != nil {
		return Migration{}, err
	}
	historyID, err := s.startHistory(ctx, databaseID, version, userID, "rollback", item.SQLDown, cancelledExecutionID)
	if err != nil {
		return Migration{}, err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := lockMigrations(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if _, err := tx.Exec(ctx, item.SQLDown); err != nil {
		_ = tx.Rollback(ctx)
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, ErrMigrationFailed
	}
	if err := tx.Commit(ctx); err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := s.markHistory(ctx, historyID, "success", ""); err != nil {
		return Migration{}, err
	}
	if _, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'rolled_back', active = FALSE, executed_by = $3, executed_at = NOW(), error_message = NULL
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID); err != nil {
		return Migration{}, ErrMigrationStore
	}
	item.Status = "rolled_back"
	item.Active = false
	item.ExecutedBy = &userID
	now := time.Now()
	item.ExecutedAt = &now
	return item, nil
}

func (s *Service) Forward(ctx context.Context, databaseID database.DatabaseID, version, userID int64) (Migration, error) {
	if version <= 0 || userID <= 0 {
		return Migration{}, ErrInvalidVersion
	}
	if s.internalDB == nil {
		return Migration{}, ErrMigrationStore
	}
	item, err := s.Get(ctx, databaseID, version)
	if err != nil {
		return Migration{}, err
	}
	if item.Active {
		return Migration{}, ErrMigrationLocked
	}
	if item.Status != "success" && item.Status != "rolled_back" && item.Status != "failed" {
		return Migration{}, ErrMigrationLocked
	}
	var latest int64
	if err := s.internalDB.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM migrations WHERE database_id = $1 AND active = TRUE`,
		databaseID).Scan(&latest); err != nil {
		return Migration{}, ErrMigrationStore
	}
	next, err := s.nextForwardVersion(ctx, databaseID, latest)
	if err != nil {
		return Migration{}, err
	}
	if version != next {
		return Migration{}, ErrForwardNotNext
	}

	historyID, err := s.startHistory(ctx, databaseID, version, userID, "forward", item.SQL, nil)
	if err != nil {
		return Migration{}, err
	}
	pool, err := s.pools.Pool(ctx, databaseID)
	if err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := lockMigrations(ctx, tx); err != nil {
		_ = tx.Rollback(ctx)
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if _, err := tx.Exec(ctx, item.SQL); err != nil {
		_ = tx.Rollback(ctx)
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, ErrMigrationFailed
	}
	if err := tx.Commit(ctx); err != nil {
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		_ = s.markFailed(ctx, databaseID, version, userID, err.Error())
		return Migration{}, database.ErrDatabaseUnavailable
	}
	if err := s.markHistory(ctx, historyID, "success", ""); err != nil {
		return Migration{}, err
	}
	if _, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'success', active = TRUE, executed_by = $3, executed_at = NOW(), error_message = NULL
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID); err != nil {
		return Migration{}, ErrMigrationStore
	}
	item.Status = "success"
	item.Active = true
	item.ExecutedBy = &userID
	now := time.Now()
	item.ExecutedAt = &now
	return item, nil
}

// Draft versions reserve numbers without changing the target database.
func (s *Service) nextForwardVersion(ctx context.Context, databaseID database.DatabaseID, latestActive int64) (int64, error) {
	var version int64
	err := s.internalDB.QueryRowContext(ctx, `SELECT COALESCE(MIN(version),0) FROM migrations
        WHERE database_id=$1 AND version>$2 AND NOT active
          AND status IN ('success', 'rolled_back', 'failed')`, databaseID, latestActive).Scan(&version)
	if err != nil {
		return 0, ErrMigrationStore
	}
	return version, nil
}

func (s *Service) ListHistory(ctx context.Context, databaseID database.DatabaseID, hideRollback bool) ([]HistoryEntry, error) {
	if s.internalDB == nil {
		return nil, ErrMigrationStore
	}
	rows, err := s.internalDB.QueryContext(ctx, `
		SELECT execution_id, version, user_id, direction, status, sql_text,
		       execution_started_at, executed_at, error_message, cancelled_execution_id
		FROM migration_history
		WHERE database_id = $1
		ORDER BY execution_id DESC`, databaseID)
	if err != nil {
		return nil, ErrMigrationStore
	}
	defer rows.Close()
	result := make([]HistoryEntry, 0)
	hidden := make(map[int64]bool)
	for rows.Next() {
		var item HistoryEntry
		var executedAt sql.NullTime
		var errorMessage sql.NullString
		var cancelled sql.NullInt64
		if err := rows.Scan(
			&item.ExecutionID,
			&item.Version,
			&item.UserID,
			&item.Direction,
			&item.Status,
			&item.SQL,
			&item.StartedAt,
			&executedAt,
			&errorMessage,
			&cancelled,
		); err != nil {
			return nil, ErrMigrationStore
		}
		if executedAt.Valid {
			item.ExecutedAt = &executedAt.Time
		}
		if errorMessage.Valid {
			item.ErrorMessage = errorMessage.String
		}
		if cancelled.Valid {
			item.CancelledExecutionID = &cancelled.Int64
		}
		if hideRollback && item.Direction == "rollback" && item.Status == "success" && cancelled.Valid {
			hidden[item.CancelledExecutionIDValue()] = true
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrMigrationStore
	}
	if !hideRollback {
		return result, nil
	}
	filtered := make([]HistoryEntry, 0, len(result))
	for _, item := range result {
		if hidden[item.ExecutionID] {
			continue
		}
		if item.Direction == "rollback" && item.Status == "success" && item.CancelledExecutionID != nil {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered, nil
}

func (s *Service) History(ctx context.Context, databaseID database.DatabaseID, hideRollback bool) ([]HistoryEntry, error) {
	return s.ListHistory(ctx, databaseID, hideRollback)
}

func (h HistoryEntry) CancelledExecutionIDValue() int64 {
	if h.CancelledExecutionID == nil {
		return 0
	}
	return *h.CancelledExecutionID
}

func (s *Service) latestUncancelledExecution(ctx context.Context, databaseID database.DatabaseID, version int64) (*int64, error) {
	var executionID int64
	err := s.internalDB.QueryRowContext(ctx, `
		SELECT h.execution_id
		FROM migration_history h
		WHERE h.database_id = $1 AND h.version = $2
		  AND h.status = 'success'
		  AND h.direction IN ('up', 'forward')
		  AND NOT EXISTS (
		      SELECT 1 FROM migration_history r
		      WHERE r.cancelled_execution_id = h.execution_id
		        AND r.status = 'success'
		  )
		ORDER BY h.execution_id DESC
		LIMIT 1`, databaseID, version).Scan(&executionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrMigrationStore
	}
	return &executionID, nil
}

func (s *Service) startHistory(
	ctx context.Context,
	databaseID database.DatabaseID,
	version, userID int64,
	direction, sqlText string,
	cancelledExecutionID *int64,
) (int64, error) {
	var executionID int64
	err := s.internalDB.QueryRowContext(ctx, `
		INSERT INTO migration_history
		    (database_id, version, user_id, direction, status, sql_text, cancelled_execution_id)
		VALUES ($1, $2, $3, $4, 'running', $5, $6)
		RETURNING execution_id`,
		databaseID, version, userID, direction, sqlText, cancelledExecutionID,
	).Scan(&executionID)
	if err != nil {
		return 0, ErrMigrationStore
	}
	return executionID, nil
}

func (s *Service) markHistory(ctx context.Context, executionID int64, status, errorMessage string) error {
	_, err := s.internalDB.ExecContext(ctx, `
		UPDATE migration_history
		SET status = $2,
		    executed_at = NOW(),
		    error_message = NULLIF($3, '')
		WHERE execution_id = $1`,
		executionID, status, errorMessage,
	)
	if err != nil {
		return ErrMigrationStore
	}
	return nil
}

func (s *Service) startTrace(
	ctx context.Context,
	name string,
	fields observability.Fields,
) (context.Context, observability.Span) {
	if s.observer == nil {
		return ctx, nil
	}
	return s.observer.StartTrace(ctx, name, fields)
}

func endTrace(span observability.Span, err error) {
	if span != nil {
		span.End(err)
	}
}

func (s *Service) log(ctx context.Context, level slog.Level, event string, fields observability.Fields) {
	if s.observer != nil {
		s.observer.Log(ctx, level, event, fields)
	}
}

func (s *Service) rollbackTarget(
	ctx context.Context,
	tx pgx.Tx,
	databaseID database.DatabaseID,
	version, userID int64,
) {
	rollbackContext, span := s.startTrace(ctx, observability.SpanMigrationRollbackTarget, observability.Fields{
		"database.id": int64(databaseID),
	})
	startedAt := time.Now()
	err := tx.Rollback(rollbackContext)
	status := "success"
	level := slog.LevelInfo
	fields := observability.Fields{
		"actor":             userID,
		"database":          int64(databaseID),
		"migration.version": version,
		"duration_ms":       time.Since(startedAt).Milliseconds(),
	}
	if err != nil {
		status = "failed"
		level = slog.LevelError
		fields["error"] = err.Error()
		if state := sqlState(err); state != "" {
			fields["sqlstate"] = state
		}
	}
	fields["status"] = status
	s.log(rollbackContext, level, observability.EventRollback, fields)
	endTrace(span, err)
}

func sqlState(err error) string {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		return postgresError.Code
	}
	return ""
}

func (s *Service) recordPoolStats(pool *pgxpool.Pool) {
	if s.observer == nil {
		return
	}
	stats := pool.Stat()
	s.observer.SetDatabasePool(stats.AcquiredConns(), stats.IdleConns())
}

const migrationColumns = `version, sql_text, sql_down, reversible, irreversible_reason, active, created_by, executed_by, created_at, executed_at, status, error_message`
const migrationSelect = `SELECT ` + migrationColumns + ` FROM migrations`

type rowScanner interface {
	Scan(...any) error
}

func scanMigration(row rowScanner, optional ...*Migration) (Migration, error) {
	var item Migration
	var errorMessage sql.NullString
	var sqlDown sql.NullString
	var irreversibleReason sql.NullString
	err := row.Scan(
		&item.Version,
		&item.SQL,
		&sqlDown,
		&item.Reversible,
		&irreversibleReason,
		&item.Active,
		&item.CreatedBy,
		&item.ExecutedBy,
		&item.CreatedAt,
		&item.ExecutedAt,
		&item.Status,
		&errorMessage,
	)
	if sqlDown.Valid {
		item.SQLDown = sqlDown.String
		item.DownSQL = sqlDown.String
	}
	if irreversibleReason.Valid {
		item.IrreversibleReason = irreversibleReason.String
	}
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
	ctx, span := s.startTrace(ctx, observability.SpanMigrationMarkSuccess, observability.Fields{
		"database.id":       int64(databaseID),
		"migration.version": version,
	})
	_, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'success', active = TRUE, executed_by = $3, executed_at = NOW(), error_message = NULL
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID)
	endTrace(span, err)
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
	ctx, span := s.startTrace(ctx, observability.SpanMigrationMarkFailed, observability.Fields{
		"database.id":       int64(databaseID),
		"migration.version": version,
	})
	_, err := s.internalDB.ExecContext(ctx, `
		UPDATE migrations
		SET status = 'failed', executed_by = $3, executed_at = NOW(), error_message = $4
		WHERE database_id = $1 AND version = $2`, databaseID, version, userID, message)
	endTrace(span, err)
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

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
