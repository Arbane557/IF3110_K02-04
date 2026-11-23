package migration

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/propagation"
	"wbd/backend/auth"
	"wbd/backend/database"
)

var (
	errJobNotFound    = errors.New("migration job not found")
	errJobConflict    = errors.New("migration job is not eligible for this action")
	errJobInput       = errors.New("invalid mode, batch table, size, or SQL")
	batchTablePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z_][a-z0-9_]*)?$`)
	statementStart    = regexp.MustCompile(`(?i)^([a-z]+)\b`)
)

type BatchOptions struct {
	Mode  string `json:"mode"`
	Table string `json:"batchTable"`
	Size  int    `json:"batchSize"`
}

type Job struct {
	ID                      int64               `json:"jobId"`
	DatabaseID              database.DatabaseID `json:"databaseId"`
	Version                 int64               `json:"version"`
	Direction               string              `json:"direction"`
	Mode                    string              `json:"mode"`
	Status                  string              `json:"status"`
	BatchesDone             int64               `json:"batchesDone"`
	RowsDone                int64               `json:"rowsDone"`
	ErrorMessage            string              `json:"errorMessage,omitempty"`
	CanApprove              bool                `json:"canApprove"`
	blocksDatabase          bool
	key                     string
	actor                   int64
	query, down, table      string
	size                    int
	source, execution       sql.NullInt64
	cancelledExecution      sql.NullInt64
	attempts, generation    int
	traceparent, tracestate string
}

func (s *Service) CanApproveJob(ctx context.Context, job Job, actor int64) (bool, error) {
	var canApprove bool
	err := s.internalDB.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM groups g
		JOIN group_database_permissions p ON p.group_id = g.id
		WHERE p.database_id = $1
		  AND g.leader_user_id = $2
		  AND $3 = 'pending_review'
		  AND NOT EXISTS (
			SELECT 1 FROM migration_job_approvals a
			WHERE a.job_id = $4 AND a.generation = $5
			  AND a.group_id = g.id AND a.leader_id = $2
		  )
	)`, job.DatabaseID, actor, job.Status, job.ID, job.generation).Scan(&canApprove)
	return canApprove, err
}

const jobColumns = `id,database_id,version,direction,mode,status,batches_done,rows_done,error_message,
    actor,sql_text,sql_down,batch_table,batch_size,source_job,execution_id,cancelled_execution_id,attempts,generation,traceparent,tracestate,job_key,blocks_database`

func scanJob(row rowScanner) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.DatabaseID, &j.Version, &j.Direction, &j.Mode, &j.Status, &j.BatchesDone, &j.RowsDone, &j.ErrorMessage,
		&j.actor, &j.query, &j.down, &j.table, &j.size, &j.source, &j.execution, &j.cancelledExecution,
		&j.attempts, &j.generation, &j.traceparent, &j.tracestate, &j.key, &j.blocksDatabase)
	if errors.Is(err, sql.ErrNoRows) {
		err = errJobNotFound
	}
	return j, err
}

func traceHeaders(ctx context.Context) propagation.MapCarrier {
	headers := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, headers)
	return headers
}

func (s *Service) SubmitJob(ctx context.Context, db database.DatabaseID, version, actor int64, direction string, opts BatchOptions) (Job, error) {
	m, err := s.Get(ctx, db, version)
	if err != nil {
		return Job{}, err
	}
	if direction == "rollback" && !m.Reversible {
		return Job{}, ErrIrreversible
	}
	var source sql.NullInt64
	if direction != "up" {
		prior, err := scanJob(s.internalDB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs
            WHERE database_id=$1 AND version=$2 AND direction IN ('up','forward') AND (status='success' OR ($3='forward' AND status='failed'))
            ORDER BY id DESC LIMIT 1`, db, version, direction))
		if err != nil && !errors.Is(err, errJobNotFound) {
			return Job{}, err
		}
		if err == nil && prior.Mode == "batch" {
			opts = BatchOptions{prior.Mode, prior.table, prior.size}
			if direction == "rollback" {
				source = sql.NullInt64{Int64: prior.ID, Valid: true}
			}
		}
	}
	if opts.Mode == "" {
		opts.Mode = "normal"
	}
	query := m.SQL
	if direction == "rollback" {
		query = m.SQLDown
	}
	if err := validateJobSQL(query, m.SQLDown, direction, opts); err != nil {
		return Job{}, err
	}
	if opts.Mode == "normal" {
		opts.Table, opts.Size = "", 0
	}
	headers := traceHeaders(ctx)
	queryCtx, span := s.startTrace(ctx, "migration.queue.submit", nil)
	job, err := scanJob(s.internalDB.QueryRowContext(queryCtx, `INSERT INTO migration_jobs
        (database_id,version,actor,direction,mode,status,sql_text,sql_down,batch_table,batch_size,source_job,traceparent,tracestate)
        SELECT $1,$2,$3,$4,$5,CASE WHEN EXISTS(SELECT 1 FROM group_database_permissions WHERE database_id=$1)
        THEN 'pending_review' ELSE 'pending' END,$6,$7,$8,$9,$10,$11,$12
        FROM migrations WHERE database_id=$1 AND version=$2 AND sql_text=$13 AND COALESCE(sql_down,'')=$7
        RETURNING `+jobColumns, db, version, actor, direction, opts.Mode, query, m.SQLDown, opts.Table, opts.Size, source,
		headers["traceparent"], headers["tracestate"], m.SQL))
	endTrace(span, err)
	if sqlState(err) == "23505" {
		return Job{}, errJobConflict
	}
	return job, err
}

func validateJobSQL(query, down, direction string, opts BatchOptions) error {
	statements, err := splitMigrationSQL(query)
	if err != nil || len(statements) == 0 {
		return errJobInput
	}
	for _, statement := range statements {
		start := statementStart.FindStringSubmatch(statement.log)
		if len(start) == 0 {
			return errJobInput
		}
		switch strings.ToUpper(start[1]) {
		case "ALTER", "CREATE", "DROP", "TRUNCATE", "COMMENT", "INSERT", "UPDATE", "DELETE", "MERGE", "WITH",
			"SELECT", "GRANT", "REVOKE", "DO", "CALL", "ANALYZE", "REFRESH", "LOCK", "SECURITY", "VALUES":
		default:
			return errJobInput
		}
	}
	if opts.Mode == "normal" {
		return nil
	}
	if len(statements) != 1 {
		return errJobInput
	}
	if down != "" {
		downStatements, err := splitMigrationSQL(down)
		if err != nil || len(downStatements) != 1 {
			return errJobInput
		}
	}
	if opts.Mode != "batch" || !batchTablePattern.MatchString(opts.Table) || opts.Size < 1 || opts.Size > 10000 {
		return errJobInput
	}
	pattern := regexp.MustCompile(`(?is)^\s*UPDATE\s+` + regexp.QuoteMeta(opts.Table) + `\s+SET\s+.+\s+WHERE\s+id\s*=\s*ANY\(\$1::bigint\[\]\)\s*;?\s*$`)
	if !pattern.MatchString(query) || (direction != "rollback" && down != "" && !pattern.MatchString(down)) {
		return errJobInput
	}
	return nil
}

func (s *Service) GetJob(ctx context.Context, db database.DatabaseID, id int64) (Job, error) {
	return scanJob(s.internalDB.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=$2`, db, id))
}

func (s *Service) ListJobs(ctx context.Context, db database.DatabaseID) ([]Job, error) {
	rows, err := s.internalDB.QueryContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 ORDER BY id DESC`, db)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func approvalsComplete(ctx context.Context, store interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, job Job) (bool, error) {
	var complete bool
	err := store.QueryRowContext(ctx, `SELECT NOT EXISTS (
        SELECT 1 FROM group_database_permissions p JOIN groups g ON g.id=p.group_id
        WHERE p.database_id=$1 AND NOT EXISTS (
            SELECT 1 FROM migration_job_approvals a WHERE a.job_id=$2 AND a.generation=$3
              AND a.group_id=g.id AND a.leader_id=g.leader_user_id))`, job.DatabaseID, job.ID, job.generation).Scan(&complete)
	return complete, err
}

func (s *Service) ApproveJob(ctx context.Context, db database.DatabaseID, id, actor int64) (Job, error) {
	tx, err := s.internalDB.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM migration_jobs WHERE database_id=$1 AND id=$2 FOR UPDATE`, db, id))
	if err != nil {
		return Job{}, err
	}
	if job.Status != "pending_review" {
		return Job{}, errJobConflict
	}
	var leader bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups g JOIN group_database_permissions p ON p.group_id=g.id
        WHERE p.database_id=$1 AND g.leader_user_id=$2)`, db, actor).Scan(&leader)
	if err != nil {
		return Job{}, err
	}
	if !leader {
		return Job{}, auth.ErrForbidden
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_job_approvals (job_id,generation,group_id,leader_id)
        SELECT $1,$2,g.id,g.leader_user_id FROM groups g JOIN group_database_permissions p ON p.group_id=g.id
        WHERE p.database_id=$3 AND g.leader_user_id=$4 ON CONFLICT DO NOTHING`, id, job.generation, db, actor)
	if err != nil {
		return Job{}, err
	}
	complete, err := approvalsComplete(ctx, tx, job)
	if err != nil {
		return Job{}, err
	}
	if complete {
		if _, err = tx.ExecContext(ctx, `UPDATE migration_jobs SET status='pending',published_at=NULL,available_at=now(),queued_at=now() WHERE id=$1`, id); err != nil {
			return Job{}, err
		}
		job.Status = "pending"
	}
	return job, tx.Commit()
}

func (s *Service) RecoverJob(ctx context.Context, db database.DatabaseID, id, actor int64, cancel bool) (Job, error) {
	headers := traceHeaders(ctx)
	return scanJob(s.internalDB.QueryRowContext(ctx, `UPDATE migration_jobs SET
       actor=$3,direction=CASE WHEN $4 THEN 'cancel' ELSE direction END,
       source_job=CASE WHEN $4 THEN id ELSE source_job END,
       cancelled_execution_id=CASE WHEN $4 THEN COALESCE(cancelled_execution_id,execution_id) ELSE cancelled_execution_id END,
       sql_text=CASE WHEN $4 THEN sql_down ELSE sql_text END,
       status=CASE WHEN EXISTS(SELECT 1 FROM group_database_permissions WHERE database_id=$1)
           THEN 'pending_review' ELSE 'pending' END,
       generation=generation+1, attempts=0, error_message='', available_at=now(), queued_at=now(),lock_wait_started_at=NULL,published_at=NULL,
       traceparent=$5,tracestate=$6
       WHERE database_id=$1 AND id=$2 AND
       ( (NOT $4 AND status IN ('partial','partially_reverted','uncertain'))
         OR ($4 AND status='partial' AND mode='batch' AND direction IN ('up','forward') AND sql_down<>'' AND batches_done>0) )
       RETURNING `+jobColumns, db, id, actor, cancel, headers["traceparent"], headers["tracestate"]))
}
