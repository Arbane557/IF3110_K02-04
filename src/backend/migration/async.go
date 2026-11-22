package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"wbd/backend/database"
	"wbd/backend/observability"
)

// Job is a durable request to execute an immutable migration revision.
type Job struct {
	ID           int64               `json:"jobId"`
	DatabaseID   database.DatabaseID `json:"databaseId"`
	Version      int64               `json:"version"`
	UserID       int64               `json:"userId"`
	Direction    string              `json:"direction"`
	Mode         string              `json:"mode"`
	BatchTable   string              `json:"batchTable,omitempty"`
	BatchSize    int                 `json:"batchSize,omitempty"`
	SQL          string              `json:"-"`
	SourceJobID  *int64              `json:"sourceJobId,omitempty"`
	Traceparent  string              `json:"-"`
	Tracestate   string              `json:"-"`
	Status       string              `json:"status"`
	Attempt      int                 `json:"attempt"`
	BatchesDone  int64               `json:"batchesDone"`
	RowsDone     int64               `json:"rowsDone"`
	ErrorMessage string              `json:"errorMessage,omitempty"`
}

var batchIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z_0-9]*$`)
var batchPredicate = regexp.MustCompile(`(?i)\bwhere\s+id\s*=\s*any\s*\(\s*\$1\s*::\s*bigint\[\]\s*\)\s*;?\s*$`)
var ErrJobBusy = errors.New("database has an unfinished migration job")
var ErrInvalidBatch = errors.New("batch requires a table with a bigint id and SQL using $1::bigint[]")
var ErrApprovalRequired = errors.New("approval from every database group leader is required")

func insertApprovalRequirements(ctx context.Context, tx *sql.Tx, jobID int64, id database.DatabaseID) error {
	result, err := tx.ExecContext(ctx, `INSERT INTO migration_job_approvals(job_id,group_id,leader_user_id)
 SELECT $1,g.id,g.leader_user_id FROM groups g JOIN group_database_permissions p ON p.group_id=g.id
 WHERE p.database_id=$2`, jobID, id)
	if err != nil {
		return ErrMigrationStore
	}
	n, err := result.RowsAffected()
	if err != nil {
		return ErrMigrationStore
	}
	if n == 0 {
		return ErrApprovalRequired
	}
	return nil
}

func (s *Service) ApproveJob(ctx context.Context, id database.DatabaseID, jobID, userID int64) (Job, error) {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx, `SELECT status FROM migration_jobs WHERE id=$1 AND database_id=$2 FOR UPDATE`, jobID, id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrMigrationNotFound
	}
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	if status != "pending_review" {
		return Job{}, ErrMigrationLocked
	}
	result, err := tx.ExecContext(ctx, `UPDATE migration_job_approvals SET approved_at=NOW() WHERE job_id=$1 AND leader_user_id=$2`, jobID, userID)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return Job{}, ErrApprovalRequired
	}
	var ready bool
	err = tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM migration_job_approvals WHERE job_id=$1 AND approved_at IS NULL)`, jobID).Scan(&ready)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	if ready {
		_, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status='pending',available_at=NOW() WHERE id=$1`, jobID)
		if err != nil {
			return Job{}, ErrMigrationStore
		}
	}
	if err = tx.Commit(); err != nil {
		return Job{}, ErrMigrationStore
	}
	return s.GetJob(ctx, id, jobID)
}

func (s *Service) approvalsStillValid(ctx context.Context, j Job) error {
	var valid bool
	err := s.internalDB.QueryRowContext(ctx, `SELECT
 EXISTS(SELECT 1 FROM users u WHERE u.id=$3 AND (u.is_admin OR EXISTS(
 SELECT 1 FROM group_members gm JOIN group_database_permissions p ON p.group_id=gm.group_id
 WHERE gm.user_id=u.id AND p.database_id=$2 AND p.access_level='edit')))
 AND
 EXISTS(SELECT 1 FROM migration_job_approvals WHERE job_id=$1)
 AND NOT EXISTS(SELECT 1 FROM migration_job_approvals a WHERE a.job_id=$1 AND
 (a.approved_at IS NULL OR NOT EXISTS(SELECT 1 FROM groups g JOIN group_database_permissions p ON p.group_id=g.id
	 WHERE g.id=a.group_id AND g.leader_user_id=a.leader_user_id AND p.database_id=$2)))
 AND NOT EXISTS(SELECT 1 FROM group_database_permissions p WHERE p.database_id=$2 AND
	 NOT EXISTS(SELECT 1 FROM migration_job_approvals a WHERE a.job_id=$1 AND a.group_id=p.group_id))`, j.ID, j.DatabaseID, j.UserID).Scan(&valid)
	if err != nil {
		return ErrMigrationStore
	}
	if !valid {
		return ErrApprovalRequired
	}
	item, err := s.Get(ctx, j.DatabaseID, j.Version)
	if err != nil {
		return err
	}
	expected := item.SQL
	if j.Direction == "rollback" {
		expected = item.SQLDown
	}
	if expected != j.SQL {
		return ErrApprovalRequired
	}
	return nil
}

// Enqueue snapshots the SQL. Approval must be checked by the caller before this method.
func (s *Service) Enqueue(ctx context.Context, id database.DatabaseID, version, userID int64, direction, mode, table string, size int) (Job, error) {
	ctx, enqueueSpan := s.startTrace(ctx, "migration.queue.publish", observability.Fields{"database.id": int64(id), "migration.version": version, "direction": direction})
	defer endTrace(enqueueSpan, nil)
	if s.internalDB == nil {
		return Job{}, ErrMigrationStore
	}
	if mode == "" {
		mode = "normal"
	}
	if mode != "normal" && mode != "batch" {
		return Job{}, ErrInvalidBatch
	}
	if mode == "batch" && (!batchIdentifier.MatchString(table) || size < 1 || size > 10000) {
		return Job{}, ErrInvalidBatch
	}
	item, err := s.Get(ctx, id, version)
	if err != nil {
		return Job{}, err
	}
	if direction == "up" && item.Status != "draft" {
		return Job{}, ErrMigrationLocked
	}
	if direction == "rollback" && (!item.Active || !item.Reversible) {
		return Job{}, ErrMigrationLocked
	}
	if direction == "forward" && item.Active {
		return Job{}, ErrMigrationLocked
	}
	if direction == "rollback" || direction == "forward" {
		var latest int64
		if err := s.internalDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations WHERE database_id=$1 AND active=TRUE`, id).Scan(&latest); err != nil {
			return Job{}, ErrMigrationStore
		}
		if direction == "rollback" && version != latest {
			return Job{}, ErrNotLatestActive
		}
		if direction == "forward" && version != latest+1 {
			return Job{}, ErrForwardNotNext
		}
	}
	query := item.SQL
	if direction == "rollback" {
		query = item.SQLDown
	}
	var source *int64
	if direction == "rollback" {
		var sourceMode, sourceTable string
		var sourceSize int
		var sourceID int64
		err = s.internalDB.QueryRowContext(ctx, `SELECT id,mode,COALESCE(batch_table,''),COALESCE(batch_size,0) FROM migration_jobs
 WHERE database_id=$1 AND version=$2 AND direction IN ('up','forward') AND status='success' ORDER BY id DESC LIMIT 1`, id, version).Scan(&sourceID, &sourceMode, &sourceTable, &sourceSize)
		if err == nil && sourceMode == "batch" {
			mode = "batch"
			table = sourceTable
			size = sourceSize
			source = &sourceID
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrMigrationStore
		}
	}
	if mode == "batch" && (!batchPredicate.MatchString(query) || !regexp.MustCompile(`(?i)^\s*update\s+`+regexp.QuoteMeta(table)+`\s+set\s+`).MatchString(query)) {
		return Job{}, ErrInvalidBatch
	}
	if mode == "batch" && regexp.MustCompile(`(?i)\bset\s+id\s*=`).MatchString(query) {
		return Job{}, ErrInvalidBatch
	}
	if mode == "normal" {
		table = ""
		size = 0
	}
	var job Job
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_jobs
	   (database_id,version,user_id,direction,mode,batch_table,batch_size,sql_text,source_job_id,traceparent,tracestate)
	   VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,0),$8,$9,$10,$11) RETURNING id`, id, version, userID, direction, mode, table, size, query, source, carrier.Get("traceparent"), carrier.Get("tracestate")).Scan(&job.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Job{}, ErrJobBusy
		}
		return Job{}, ErrMigrationStore
	}
	if err = insertApprovalRequirements(ctx, tx, job.ID, id); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, ErrMigrationStore
	}
	job.DatabaseID = id
	job.Version = version
	job.UserID = userID
	job.Direction = direction
	job.Mode = mode
	job.BatchTable = table
	job.BatchSize = size
	job.Status = "pending_review"
	job.SourceJobID = source
	job.Traceparent = carrier.Get("traceparent")
	job.Tracestate = carrier.Get("tracestate")
	return job, nil
}

const jobColumns = `id,database_id,version,user_id,direction,mode,COALESCE(batch_table,''),COALESCE(batch_size,0),sql_text,source_job_id,COALESCE(traceparent,''),COALESCE(tracestate,''),status,attempt,batches_done,rows_done,COALESCE(error_message,'')`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.DatabaseID, &j.Version, &j.UserID, &j.Direction, &j.Mode, &j.BatchTable, &j.BatchSize, &j.SQL, &j.SourceJobID, &j.Traceparent, &j.Tracestate, &j.Status, &j.Attempt, &j.BatchesDone, &j.RowsDone, &j.ErrorMessage)
	return j, err
}

func (s *Service) ListJobs(ctx context.Context, id database.DatabaseID) ([]Job, error) {
	rows, err := s.internalDB.QueryContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 ORDER BY id DESC LIMIT 100`, id)
	if err != nil {
		return nil, ErrMigrationStore
	}
	defer rows.Close()
	result := make([]Job, 0)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, ErrMigrationStore
		}
		result = append(result, j)
	}
	return result, rows.Err()
}

func (s *Service) GetJob(ctx context.Context, id database.DatabaseID, jobID int64) (Job, error) {
	j, err := scanJob(s.internalDB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=$2`, id, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrMigrationNotFound
	}
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	return j, nil
}

// Claim is atomic across worker processes. A timed-out worker is recovered only
// after the target advisory lock is acquired and the target receipt inspected.
func (s *Service) claim(ctx context.Context) (Job, error) {
	j, err := scanJob(s.internalDB.QueryRowContext(ctx, `WITH candidate AS (
 SELECT id FROM migration_jobs WHERE status='pending' AND available_at<=NOW()
 ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
 UPDATE migration_jobs SET status='running',attempt=attempt+1,claimed_at=NOW()
 WHERE id=(SELECT id FROM candidate) RETURNING `+jobColumns))
	return j, err
}

func (s *Service) recoverAbandoned(ctx context.Context) {
	// Target lock serializes recovered workers with any worker still executing.
	_, _ = s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET status='pending',available_at=NOW()
 WHERE status='running' AND claimed_at<NOW()-INTERVAL '2 minutes'`)
}

func (s *Service) RunWorker(ctx context.Context) {
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); s.workerLoop(ctx) }()
	}
	workers.Wait()
}

func (s *Service) workerLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		s.recoverAbandoned(ctx)
		j, err := s.claim(ctx)
		if err == nil {
			s.runJob(ctx, j)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Error("claim migration job", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) runJob(ctx context.Context, j Job) {
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": j.Traceparent, "tracestate": j.Tracestate})
	ctx, span := s.startTrace(ctx, "migration.worker", observability.Fields{"job.id": j.ID, "database.id": int64(j.DatabaseID), "attempt": j.Attempt})
	defer endTrace(span, nil)
	pool, err := s.pools.Pool(ctx, j.DatabaseID)
	if err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	defer conn.Release()
	// Session lock survives each batch commit; a lost connection releases it.
	lockCtx, lockSpan := s.startTrace(ctx, "migration.worker.lock", observability.Fields{"job.id": j.ID, "database.id": int64(j.DatabaseID)})
	_, err = conn.Exec(lockCtx, `SELECT pg_advisory_lock(hashtextextended('migration_execution',0))`)
	endTrace(lockSpan, err)
	if err != nil {
		s.finishJob(ctx, j, "uncertain", err)
		return
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended('migration_execution',0))`)
	}()
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS platform_job_receipts (
 job_id BIGINT NOT NULL, batch_number BIGINT NOT NULL, cursor_id BIGINT NOT NULL DEFAULT 0,
 rows_affected BIGINT NOT NULL, PRIMARY KEY(job_id,batch_number))`); err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS platform_job_scope (
 job_id BIGINT PRIMARY KEY, high_id BIGINT NOT NULL)`); err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS platform_job_scope_rows (
 job_id BIGINT NOT NULL, id BIGINT NOT NULL, batch_number BIGINT NOT NULL,
 PRIMARY KEY(job_id,id))`); err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	if _, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS platform_job_before (
 job_id BIGINT NOT NULL, id BIGINT NOT NULL, batch_number BIGINT NOT NULL,
 row_data JSONB NOT NULL, PRIMARY KEY(job_id,id))`); err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	// Another worker may have completed this job while its internal lease expired.
	current, err := s.GetJob(ctx, j.DatabaseID, j.ID)
	if err != nil || current.Status != "running" || current.Attempt != j.Attempt {
		return
	}
	// Recover progress even when a worker died after committing the target batch.
	var committedBatches, committedRows int64
	if err = conn.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE batch_number>0),COALESCE(SUM(rows_affected),0)
 FROM platform_job_receipts WHERE job_id=$1`, j.ID).Scan(&committedBatches, &committedRows); err != nil {
		s.finishJob(ctx, j, "uncertain", err)
		return
	}
	_, _ = s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET batches_done=$2,rows_done=$3 WHERE id=$1`, j.ID, committedBatches, committedRows)
	item, err := s.Get(ctx, j.DatabaseID, j.Version)
	if err != nil {
		s.finishJob(ctx, j, "uncertain", err)
		return
	}
	partialCancel := false
	if j.Direction == "rollback" && j.SourceJobID != nil {
		var sourceStatus string
		_ = s.internalDB.QueryRowContext(ctx, `SELECT status FROM migration_jobs WHERE id=$1`, *j.SourceJobID).Scan(&sourceStatus)
		partialCancel = sourceStatus == "cancelled"
	}
	if (j.Direction == "up" && item.Status != "draft" && item.Status != "running") || (j.Direction == "rollback" && !item.Active && !partialCancel) || (j.Direction == "forward" && item.Active) {
		s.finishJob(ctx, j, "failed", ErrMigrationLocked)
		return
	}
	if j.Direction == "rollback" || j.Direction == "forward" {
		var latest int64
		if err = s.internalDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM migrations WHERE database_id=$1 AND active=TRUE`, j.DatabaseID).Scan(&latest); err != nil {
			s.finishJob(ctx, j, "uncertain", err)
			return
		}
		if (j.Direction == "rollback" && !partialCancel && j.Version != latest) || (j.Direction == "forward" && j.Version != latest+1) {
			s.finishJob(ctx, j, "failed", ErrMigrationLocked)
			return
		}
	}
	if err = s.approvalsStillValid(ctx, j); err != nil {
		s.finishJob(ctx, j, "failed", err)
		return
	}
	// Keep each attempt, including a retry, in the append-only history.
	direction := j.Direction
	var cancelled *int64
	if direction == "rollback" {
		cancelled, err = s.latestUncancelledExecution(ctx, j.DatabaseID, j.Version)
		if err != nil {
			s.finishJob(ctx, j, "uncertain", err)
			return
		}
	}
	historyID, historyErr := s.startHistory(ctx, j.DatabaseID, j.Version, j.UserID, direction, j.SQL, cancelled)
	if historyErr != nil {
		s.finishJob(ctx, j, "uncertain", historyErr)
		return
	}
	ctx, batchSpan := s.startTrace(ctx, "migration.worker.query", observability.Fields{"job.id": j.ID, "direction": j.Direction, "query": j.SQL})
	if j.Mode == "batch" {
		if j.Direction == "rollback" {
			err = s.compensateBatches(ctx, conn.Conn(), j)
		} else {
			err = s.executeBatches(ctx, conn.Conn(), j)
		}
	} else {
		err = s.executeNormal(ctx, conn.Conn(), j)
	}
	endTrace(batchSpan, err)
	if err != nil {
		slog.Error("migration job failed", "job", j.ID, "error", err)
		status := "partial"
		if j.Direction == "rollback" && j.Mode == "batch" {
			status = "partially_reverted"
		}
		if j.Mode == "normal" {
			status = "uncertain"
		}
		state := sqlState(err)
		if j.Attempt < 3 && (state == "40P01" || state == "55P03" || (len(state) >= 2 && state[:2] == "08")) {
			status = "pending"
		}
		_ = s.markHistory(ctx, historyID, "failed", err.Error())
		s.finishJob(ctx, j, status, err)
		return
	}
	_ = s.markHistory(ctx, historyID, "success", "")
	s.finishJob(ctx, j, "success", nil)
}

func (s *Service) executeNormal(ctx context.Context, c *pgx.Conn, j Job) error {
	var exists bool
	if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_job_receipts WHERE job_id=$1 AND batch_number=0)`, j.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, j.SQL)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_job_receipts(job_id,batch_number,rows_affected) VALUES($1,0,$2)`, j.ID, tag.RowsAffected()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Batch SQL must constrain the operation to id = ANY($1::bigint[]).
// The cursor and receipt are committed in the same target transaction as SQL.
func (s *Service) executeBatches(ctx context.Context, c *pgx.Conn, j Job) error {
	table := pgx.Identifier{j.BatchTable}.Sanitize()
	// Snapshot the complete ID set in the target, without loading it into Go memory.
	// A restart or concurrent insert cannot change the batches already selected.
	var scoped bool
	if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_job_scope WHERE job_id=$1)`, j.ID).Scan(&scoped); err != nil {
		return err
	}
	if !scoped {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `INSERT INTO platform_job_scope(job_id,high_id) SELECT $1,COALESCE(MAX(id),0) FROM `+table, j.ID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO platform_job_scope_rows(job_id,id,batch_number)
 SELECT $1,id,(row_number() OVER (ORDER BY id)-1)/$2+1 FROM `+table, j.ID, j.BatchSize); err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
	}
	var batch int64
	if err := c.QueryRow(ctx, `SELECT COALESCE(MAX(batch_number),0) FROM platform_job_receipts WHERE job_id=$1`, j.ID).Scan(&batch); err != nil {
		return err
	}
	for {
		rows, err := c.Query(ctx, `SELECT id FROM platform_job_scope_rows WHERE job_id=$1 AND batch_number=$2 ORDER BY id`, j.ID, batch+1)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		beforeTag, err := tx.Exec(ctx, `INSERT INTO platform_job_before(job_id,id,batch_number,row_data)
 SELECT $1,t.id,$2,to_jsonb(t) FROM `+table+` t WHERE t.id=ANY($3::bigint[])`, j.ID, batch+1, ids)
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if beforeTag.RowsAffected() != int64(len(ids)) {
			_ = tx.Rollback(ctx)
			return ErrInvalidBatch
		}
		tag, err := tx.Exec(ctx, j.SQL, ids)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO platform_job_receipts(job_id,batch_number,cursor_id,rows_affected) VALUES($1,$2,$3,$4)`, j.ID, batch+1, ids[len(ids)-1], tag.RowsAffected())
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		batch++
		_, _ = s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET batches_done=$2,rows_done=rows_done+$3,claimed_at=NOW() WHERE id=$1`, j.ID, batch, tag.RowsAffected())
	}
}

// Rollback compensates committed batches in reverse order, recording each
// compensation in the same transaction as the SQL down statement.
func (s *Service) compensateBatches(ctx context.Context, c *pgx.Conn, j Job) error {
	if j.SourceJobID == nil {
		return ErrInvalidBatch
	}
	for {
		var batch int64
		err := c.QueryRow(ctx, `SELECT r.batch_number FROM platform_job_receipts r
 WHERE r.job_id=$1 AND r.batch_number>0 AND NOT EXISTS
 (SELECT 1 FROM platform_job_receipts done WHERE done.job_id=$2 AND done.batch_number=r.batch_number)
 ORDER BY r.batch_number DESC LIMIT 1`, *j.SourceJobID, j.ID).Scan(&batch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		selected, err := c.Query(ctx, `SELECT id FROM platform_job_scope_rows WHERE job_id=$1 AND batch_number=$2 ORDER BY id`, *j.SourceJobID, batch)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(selected, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, j.SQL, ids)
		if err == nil {
			var identical bool
			table := pgx.Identifier{j.BatchTable}.Sanitize()
			err = tx.QueryRow(ctx, `SELECT COUNT(*)=$3 AND COALESCE(BOOL_AND(t.id IS NOT NULL AND to_jsonb(t)=b.row_data),FALSE)
 FROM platform_job_before b LEFT JOIN `+table+` t ON t.id=b.id
 WHERE b.job_id=$1 AND b.batch_number=$2`, *j.SourceJobID, batch, len(ids)).Scan(&identical)
			if err == nil && !identical {
				err = ErrInvalidBatch
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO platform_job_receipts(job_id,batch_number,rows_affected) VALUES($1,$2,$3)`, j.ID, batch, tag.RowsAffected())
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		_, _ = s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET batches_done=batches_done+1,rows_done=rows_done+$2,claimed_at=NOW() WHERE id=$1`, j.ID, tag.RowsAffected())
	}
	return nil
}

func (s *Service) finishJob(ctx context.Context, j Job, status string, runErr error) {
	// Reconcile progress from target before allowing another job on this database.
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	if status == "success" {
		if j.Direction == "up" || j.Direction == "forward" {
			if err := s.markSuccess(ctx, j.DatabaseID, j.Version, j.UserID); err != nil {
				status = "uncertain"
				message = err.Error()
			}
		}
		if j.Direction == "rollback" {
			_, err := s.internalDB.ExecContext(ctx, `UPDATE migrations SET status='rolled_back',active=FALSE,executed_by=$3,executed_at=NOW() WHERE database_id=$1 AND version=$2`, j.DatabaseID, j.Version, j.UserID)
			if err != nil {
				status = "uncertain"
				message = err.Error()
			}
		}
	}
	if status == "partial" && j.Mode == "normal" {
		status = "failed"
	}
	if status == "pending" {
		_, err := s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET status='pending',error_message=$2,
 available_at=NOW()+($3 * INTERVAL '5 seconds'),claimed_at=NULL WHERE id=$1`, j.ID, message, j.Attempt)
		if err != nil {
			slog.Error("retry migration job", "job", j.ID, "error", err)
		}
		return
	}
	if status == "failed" && j.Direction == "up" {
		_ = s.markFailed(ctx, j.DatabaseID, j.Version, j.UserID, message)
	}
	_, err := s.internalDB.ExecContext(ctx, `UPDATE migration_jobs SET status=$2,error_message=NULLIF($3,''),finished_at=NOW() WHERE id=$1`, j.ID, status, message)
	if err != nil {
		slog.Error("finish migration job", "job", j.ID, "error", err)
	}
	slog.Info("migration job finished", "job", j.ID, "database", j.DatabaseID, "direction", j.Direction, "status", status, "error", message)
}

func (s *Service) ResumeJob(ctx context.Context, id database.DatabaseID, jobID int64) error {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return ErrMigrationStore
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `UPDATE migration_jobs SET status='pending_review',available_at=NOW(),error_message=NULL,traceparent=$3,tracestate=$4 WHERE id=$1 AND database_id=$2 AND status IN ('partial','uncertain','partially_reverted')`, jobID, id, carrier.Get("traceparent"), carrier.Get("tracestate"))
	if err != nil {
		return ErrMigrationStore
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return fmt.Errorf("job cannot be resumed: %w", ErrMigrationLocked)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM migration_job_approvals WHERE job_id=$1`, jobID); err != nil {
		return ErrMigrationStore
	}
	if err = insertApprovalRequirements(ctx, tx, jobID, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return ErrMigrationStore
	}
	return nil
}

// CancelPartialBatch reserves the database for a compensating job. The source
// checkpoint and SQL down remain immutable; no committed batch is erased.
func (s *Service) CancelPartialBatch(ctx context.Context, id database.DatabaseID, jobID, userID int64) (Job, error) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	defer tx.Rollback()
	var version int64
	var table string
	var size int
	err = tx.QueryRowContext(ctx, `SELECT version,COALESCE(batch_table,''),COALESCE(batch_size,0)
 FROM migration_jobs WHERE id=$1 AND database_id=$2 AND mode='batch' AND status='partial' FOR UPDATE`, jobID, id).Scan(&version, &table, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrMigrationLocked
	}
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	var down string
	var reversible bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sql_down,''),reversible FROM migrations WHERE database_id=$1 AND version=$2`, id, version).Scan(&down, &reversible); err != nil {
		return Job{}, ErrMigrationStore
	}
	if !reversible || !batchPredicate.MatchString(down) || !regexp.MustCompile(`(?i)^\s*update\s+`+regexp.QuoteMeta(table)+`\s+set\s+`).MatchString(down) {
		return Job{}, ErrIrreversible
	}
	if _, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status='cancelled' WHERE id=$1`, jobID); err != nil {
		return Job{}, ErrMigrationStore
	}
	var result Job
	err = tx.QueryRowContext(ctx, `INSERT INTO migration_jobs(database_id,version,user_id,direction,mode,batch_table,batch_size,sql_text,source_job_id,traceparent,tracestate)
 VALUES($1,$2,$3,'rollback','batch',$4,$5,$6,$7,$8,$9) RETURNING id`, id, version, userID, table, size, down, jobID, carrier.Get("traceparent"), carrier.Get("tracestate")).Scan(&result.ID)
	if err != nil {
		return Job{}, ErrMigrationStore
	}
	if err = insertApprovalRequirements(ctx, tx, result.ID, id); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, ErrMigrationStore
	}
	result.DatabaseID = id
	result.Version = version
	result.UserID = userID
	result.Direction = "rollback"
	result.Mode = "batch"
	result.BatchTable = table
	result.BatchSize = size
	result.SourceJobID = &jobID
	result.Status = "pending_review"
	return result, nil
}
