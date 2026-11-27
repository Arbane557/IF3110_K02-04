package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/propagation"
	"wbd/backend/observability"
)

const targetLock = "hashtextextended('migration_execution', 0)"
const maxAttempts = 3

var errCommitUnknown = errors.New("target commit outcome unknown")
var errCompensation = errors.New("SQL down did not safely restore the batch")

// The advisory lock is held by this physical target connection across all commits.
func (s *Service) processJob(ctx context.Context, id int64) error {
	job, err := scanJob(s.internalDB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id=$1`, id))
	if errors.Is(err, errJobNotFound) {
		return nil
	}
	if err != nil || !jobRunnable(job) {
		return err
	}
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": job.traceparent, "tracestate": job.tracestate})
	ctx, span := s.startTrace(ctx, "migration.worker", job.fields())
	err = s.runJob(ctx, job)
	endTrace(span, traceJobError(err))
	return err
}

func jobRunnable(job Job) bool { return job.Status == "pending" || job.Status == "running" }

func (j Job) fields() observability.Fields {
	return observability.Fields{"job_id": j.ID, "execution_id": j.execution.Int64, "database": int64(j.DatabaseID),
		"migration.version": j.Version, "actor": j.actor, "attempt": j.attempts, "direction": j.Direction}
}

func (s *Service) runJob(ctx context.Context, job Job) error {
	var ready bool
	err := s.internalDB.QueryRowContext(ctx, `SELECT available_at <= now() AND NOT EXISTS (
      SELECT 1 FROM migration_jobs b WHERE b.database_id=j.database_id AND b.id<>j.id
         AND (b.blocks_database OR (NOT j.blocks_database AND b.id<j.id AND b.status='pending')))
      FROM migration_jobs j WHERE id=$1`, job.ID).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !ready {
		return s.deferJob(ctx, job)
	}
	pool, err := s.pools.Pool(ctx, job.DatabaseID)
	if err != nil {
		return s.unavailableJob(ctx, job)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return s.unavailableJob(ctx, job)
	}
	lockStarted, err := s.beginLockWait(ctx, job)
	if err != nil {
		conn.Release()
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}

	lockCtx, span := s.startTrace(ctx, "migration.target.lock", job.fields())
	var locked bool
	err = conn.QueryRow(lockCtx, "SELECT pg_try_advisory_lock("+targetLock+")").Scan(&locked)
	endTrace(span, err)
	if err != nil {
		closeJobConnection(conn)
		return s.unavailableJob(ctx, job)
	}
	if !locked {
		conn.Release()
		return s.deferJob(ctx, job)
	}
	defer closeJobConnection(conn)
	if err := s.finishLockWait(ctx, job, lockStarted); err != nil {
		return err
	}
	job, err = s.GetJob(ctx, job.DatabaseID, job.ID)
	if errors.Is(err, errJobNotFound) {
		return nil
	}
	if err != nil || !jobRunnable(job) {
		return err
	}
	// The blocker persists after the connection closes, including during reapproval.
	var blocked bool
	err = s.internalDB.QueryRowContext(ctx, `SELECT EXISTS(
        SELECT 1 FROM migration_jobs WHERE database_id=$1 AND id<>$2
         AND (blocks_database OR (NOT $3 AND id<$2 AND status='pending')))`, job.DatabaseID, job.ID, job.blocksDatabase).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return s.deferJob(ctx, job)
	}

	if err = ensureTargetMetadata(ctx, conn); err != nil {
		return s.unavailableJob(ctx, job)
	}
	done, batches, rows, err := targetProgress(ctx, conn, job)
	if err != nil {
		return s.unavailableJob(ctx, job)
	}
	job.BatchesDone, job.RowsDone = batches, rows
	// Reconcile a known commit before authorization checks or any retry.
	if done {
		return s.finishJob(ctx, job, "success", "", false)
	}

	if job.attempts >= maxAttempts {
		return s.finishJob(ctx, job, incompleteStatus(job), "Worker retry limit reached", false)
	}
	job, err = s.startAttempt(ctx, job)
	if err != nil {
		return err
	}
	if err = s.revalidateJob(ctx, job); err != nil {
		switch {
		case errors.Is(err, errApprovalChanged):
			return s.finishJob(ctx, job, "pending_review", "Approval requirements changed", false)
		case errors.Is(err, errJobConflict), errors.Is(err, ErrIrreversible), errors.Is(err, ErrNotLatestActive), errors.Is(err, ErrForwardNotNext):
			return s.finishJob(ctx, job, incompleteStatus(job), err.Error(), false)
		default:
			return s.finishJob(ctx, job, incompleteStatus(job), "Could not revalidate migration: "+err.Error(), true)
		}
	}
	err = s.executeJob(ctx, conn, job)
	if err == nil {
		_, job.BatchesDone, job.RowsDone, err = targetProgress(ctx, conn, job)
		if err != nil {
			return err
		}
		return s.finishJob(ctx, job, "success", "", false)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errCommitUnknown) {
		return s.finishJob(ctx, job, "uncertain", "Commit outcome requires verification", true)
	}
	_, batches, rows, checkErr := targetProgress(ctx, conn, job)
	if checkErr != nil {
		return s.finishJob(ctx, job, "uncertain", "Could not verify target progress", true)
	}
	job.BatchesDone, job.RowsDone = batches, rows
	message := "Migration SQL failed"
	if errors.Is(err, errCompensation) {
		message = err.Error()
	}
	return s.finishJob(ctx, job, incompleteStatus(job), message, !errors.Is(err, errCompensation) && !errors.Is(err, errJobInput) && retryable(err))
}

func closeJobConnection(conn *pgxpool.Conn) {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conn.Hijack().Close(cleanup)
}

func (s *Service) deferJob(ctx context.Context, job Job) error {
	// Do not overwrite a newer approval, attempt or retry.
	_, err := s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET published_at=NULL,
      available_at=GREATEST(available_at,now()+interval '2 seconds')
      WHERE id=$1 AND generation=$2 AND attempts=$3 AND status=$4 AND status='pending' AND published_at IS NOT NULL`,
		job.ID, job.generation, job.attempts, job.Status)
	if job.Status == "running" && err == nil {
		return errJobBusy
	}
	return err
}

var errJobBusy = errors.New("migration job lock busy")
var errApprovalChanged = errors.New("migration approval requirements changed")

func (s *Service) unavailableJob(ctx context.Context, job Job) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if job.execution.Valid {
		// Preserve the original attempt until a connection can verify its commit.
		_, err := s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET
          attempts=LEAST(attempts+1,$4), status=CASE WHEN attempts+1 >= $4 THEN 'uncertain' ELSE 'pending' END,
          published_at=NULL,available_at=now()+interval '4 seconds',error_message='Target database unavailable'
          WHERE id=$1 AND generation=$2 AND attempts=$3 AND status=$5`,
			job.ID, job.generation, job.attempts, maxAttempts, job.Status)
		return err
	}
	if job.attempts >= maxAttempts {
		status := "failed"
		if job.execution.Valid || job.BatchesDone > 0 || job.Status == "running" {
			status = "uncertain"
		}
		return s.finishJob(ctx, job, status, "Target database unavailable", false)
	}
	started, err := s.startAttempt(ctx, job)
	if err != nil {
		return err
	}
	return s.finishJob(ctx, started, "uncertain", "Target database unavailable", true)
}

func incompleteStatus(job Job) string {
	if job.Direction == "cancel" || (job.Direction == "rollback" && job.BatchesDone > 0) {
		return "partially_reverted"
	}
	if job.BatchesDone > 0 {
		return "partial"
	}
	return "failed"
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return true
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01" || pgErr.Code == "55P03" || pgErr.Code == "57P01"
}

func (s *Service) startAttempt(ctx context.Context, j Job) (Job, error) {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return j, err
	}
	defer tx.Rollback()
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id=$1 FOR UPDATE`, j.ID))
	if err != nil {
		return j, err
	}
	if current.generation != j.generation || current.attempts != j.attempts || current.Status != j.Status {
		return j, errJobBusy
	}
	if j.execution.Valid {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_history SET status='failed',executed_at=now(),error_message='Worker interrupted'
          WHERE execution_id=$1 AND status='running'`, j.execution.Int64); err != nil {
			return j, err
		}
	}
	var cancelled sql.NullInt64
	if j.Direction == "rollback" {
		err = tx.QueryRowContext(ctx, `SELECT h.execution_id FROM migration_history h WHERE h.database_id=$1 AND h.version=$2
            AND h.status='success' AND h.direction IN ('up','forward')
            AND NOT EXISTS(SELECT 1 FROM migration_history r WHERE r.cancelled_execution_id=h.execution_id AND r.status='success')
            ORDER BY h.execution_id DESC LIMIT 1`, j.DatabaseID, j.Version).Scan(&cancelled)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return j, err
		}
	} else if j.Direction == "cancel" {
		cancelled = j.cancelledExecution
		if !cancelled.Valid {
			cancelled = j.execution
		}
		if !cancelled.Valid {
			err = tx.QueryRowContext(ctx, `SELECT execution_id FROM migration_history
                WHERE database_id=$1 AND version=$2 AND direction IN ('up','forward') AND status='failed'
                  AND sql_text=$3 ORDER BY execution_id DESC LIMIT 1`, j.DatabaseID, j.Version, j.query).Scan(&cancelled)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return j, err
			}
		}
	}
	direction := j.Direction
	if direction == "cancel" {
		direction = "rollback"
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_history (database_id,version,user_id,direction,status,sql_text,cancelled_execution_id)
        VALUES ($1,$2,$3,$4,'running',$5,$6) RETURNING execution_id`, j.DatabaseID, j.Version, j.actor, direction, j.query, cancelled).Scan(&j.execution)
	if err != nil {
		return j, err
	}
	j.Status = "running"
	j.attempts++
	var queueSeconds float64
	err = tx.QueryRowContext(ctx, `UPDATE migration_jobs SET status='running',blocks_database=true,execution_id=$2,attempts=$3 WHERE id=$1
        RETURNING GREATEST(EXTRACT(EPOCH FROM clock_timestamp()-queued_at),0)::double precision`,
		j.ID, j.execution, j.attempts).Scan(&queueSeconds)
	if err != nil {
		return j, err
	}
	if err = tx.Commit(); err != nil {
		return j, err
	}
	if s.observer != nil {
		s.observer.RecordQueueWait(time.Duration(queueSeconds * float64(time.Second)))
		if j.attempts > 1 || j.generation > 1 {
			s.observer.RecordRetry()
		}
	}
	fields := j.fields()
	fields["status"] = "running"
	fields["query"] = sqlForLog(j.query)
	s.log(ctx, slog.LevelInfo, "migration.attempt.started", fields)
	return j, nil
}

func (s *Service) revalidateJob(ctx context.Context, j Job) error {
	var allowed bool
	err := s.internalDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND (u.is_admin OR EXISTS(
        SELECT 1 FROM group_members m JOIN group_database_permissions p ON p.group_id=m.group_id
        WHERE m.user_id=u.id AND p.database_id=$2 AND p.access_level='edit')))`, j.actor, j.DatabaseID).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return errJobConflict
	}
	approved, err := approvalsComplete(ctx, s.internalDB, j)
	if err != nil {
		return err
	}
	if !approved {
		return errApprovalChanged
	}
	m, err := s.Get(ctx, j.DatabaseID, j.Version)
	if err != nil {
		return err
	}
	wanted := m.SQL
	if j.Direction == "rollback" || j.Direction == "cancel" {
		wanted = m.SQLDown
	}
	if j.query != wanted || j.down != m.SQLDown {
		return errJobConflict
	}
	var latest int64
	err = s.internalDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations WHERE database_id=$1 AND active`, j.DatabaseID).Scan(&latest)
	if err != nil {
		return err
	}
	switch j.Direction {
	case "cancel":
		if !m.Reversible || m.Active {
			return ErrIrreversible
		}
	case "rollback":
		if !m.Reversible {
			return ErrIrreversible
		}
		if !m.Active || latest != j.Version {
			return ErrNotLatestActive
		}
		if j.source.Valid {
			var currentSource int64
			err = s.internalDB.QueryRowContext(ctx, `SELECT id FROM migration_jobs WHERE database_id=$1 AND version=$2
                AND direction IN ('up','forward') AND status='success' ORDER BY id DESC LIMIT 1`, j.DatabaseID, j.Version).Scan(&currentSource)
			if err != nil {
				return err
			}
			if currentSource != j.source.Int64 {
				return errJobConflict
			}
		}
	case "forward":
		next, err := s.nextForwardVersion(ctx, j.DatabaseID, latest)
		if err != nil {
			return err
		}
		if j.Version != next {
			return ErrForwardNotNext
		}
	case "up":
		if m.Active || m.Status != "draft" || j.Version <= latest {
			return errJobConflict
		}
	}
	return nil
}

func (s *Service) finishJob(ctx context.Context, j Job, status, message string, retry bool) error {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE id=$1 FOR UPDATE`, j.ID))
	if err != nil {
		return err
	}
	if current.generation != j.generation || current.attempts != j.attempts || !jobRunnable(current) {
		return nil
	}
	historyStatus := "failed"
	if status == "success" {
		historyStatus = "success"
	}
	if j.execution.Valid && status != "uncertain" {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_history SET status=$2,executed_at=now(),error_message=NULLIF($3,'')
          WHERE execution_id=$1 AND status='running'`, j.execution, historyStatus, message); err != nil {
			return err
		}
	}
	if status == "success" && j.Direction != "cancel" {
		migrationStatus := "success"
		active := j.Direction != "rollback"
		if !active {
			migrationStatus = "rolled_back"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE migrations SET active=$3,status=$4,executed_by=$5,executed_at=now(),error_message=NULL
            WHERE database_id=$1 AND version=$2`, j.DatabaseID, j.Version, active, migrationStatus, j.actor); err != nil {
			return err
		}
	} else if ((status == "failed" && !retry) || (status == "success" && j.Direction == "cancel")) && j.Direction != "rollback" {
		if _, err = tx.ExecContext(ctx, `UPDATE migrations SET status='failed',executed_by=$3,executed_at=now(),error_message=NULLIF($4,'')
             WHERE database_id=$1 AND version=$2 AND NOT active AND sql_text=$5
             AND (status='draft' OR $6)`, j.DatabaseID, j.Version, j.actor, message, j.query, status == "success"); err != nil {
			return err
		}
		if status == "success" && j.Direction == "cancel" {
			if _, err = tx.ExecContext(ctx, `UPDATE migrations SET status='failed' WHERE database_id=$1 AND version=$2 AND NOT active`, j.DatabaseID, j.Version); err != nil {
				return err
			}
		}
	}
	block := status == "partial" || status == "partially_reverted" || status == "uncertain" ||
		(status == "pending_review" && (j.BatchesDone > 0 || j.Direction == "cancel"))
	next := status
	delay := 0
	if retry && j.attempts < maxAttempts {
		next = "pending"
		block = true
		delay = 1 << j.attempts
	}
	var startedAt sql.NullTime
	if s.observer != nil && j.execution.Valid && status != "uncertain" {
		if err := tx.QueryRowContext(ctx, `SELECT execution_started_at FROM migration_history WHERE execution_id=$1`, j.execution).Scan(&startedAt); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status=$2,error_message=$3,batches_done=$4,rows_done=$5,
      blocks_database=$6,published_at=NULL,available_at=now()+$7*interval '1 second',
      queued_at=CASE WHEN $8 THEN clock_timestamp()+$7*interval '1 second' ELSE queued_at END WHERE id=$1`,
		j.ID, next, message, j.BatchesDone, j.RowsDone, block, delay, next == "pending")
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if startedAt.Valid {
		s.observer.RecordMigration(observability.MigrationResult{
			Direction: j.Direction, Status: historyStatus, Duration: time.Since(startedAt.Time),
		})
	}
	fields := j.fields()
	fields["status"], fields["query"], fields["error"] = next, sqlForLog(j.query), message
	s.log(ctx, slog.LevelInfo, "migration.attempt.finished", fields)
	return nil
}

func commitJobTransaction(ctx context.Context, tx pgx.Tx) error {
	err := tx.Commit(ctx)
	if err == nil || errors.Is(err, pgx.ErrTxCommitRollback) {
		return err
	}
	if _, ok := err.(*pgconn.PgError); ok {
		return err
	}
	return fmt.Errorf("%w: %v", errCommitUnknown, err)
}
func traceJobError(err error) error {
	if err == nil {
		return nil
	}
	if state := sqlState(err); state != "" {
		return fmt.Errorf("PostgreSQL error %s", state)
	}
	if errors.Is(err, errCommitUnknown) {
		return errCommitUnknown
	}
	if errors.Is(err, errCompensation) {
		return errCompensation
	}
	return ErrMigrationFailed
}
