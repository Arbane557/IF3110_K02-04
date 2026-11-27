package migration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"wbd/backend/observability"
)

const targetMetadata = `
CREATE TABLE IF NOT EXISTS wbd_migration_scope (
    job_key TEXT NOT NULL,
    id BIGINT NOT NULL,
    PRIMARY KEY (job_key,id)
);
CREATE TABLE IF NOT EXISTS wbd_migration_commits (
    job_key TEXT NOT NULL,
    reversing BOOLEAN NOT NULL,
    batch BIGINT NOT NULL,
    row_count BIGINT NOT NULL DEFAULT 0,
    execution_id BIGINT,
    cursor_id BIGINT,
    ids BIGINT[],
    before_rows JSONB,
    after_rows JSONB,
    PRIMARY KEY (job_key,reversing,batch)
);
ALTER TABLE wbd_migration_commits ADD COLUMN IF NOT EXISTS execution_id BIGINT;
ALTER TABLE wbd_migration_commits ADD COLUMN IF NOT EXISTS cursor_id BIGINT;`

func ensureTargetMetadata(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, targetMetadata)
	return err
}

func (j Job) reversing() bool { return j.Direction == "rollback" || j.Direction == "cancel" }

func targetProgress(ctx context.Context, conn *pgxpool.Conn, j Job) (done bool, batches, rows int64, err error) {
	err = conn.QueryRow(ctx, `SELECT COALESCE(bool_or(batch=0),false),count(*) FILTER(WHERE batch>0),
        COALESCE(sum(row_count) FILTER(WHERE batch>=0),0)
        FROM wbd_migration_commits WHERE job_key=$1 AND reversing=$2`, j.key, j.reversing()).Scan(&done, &batches, &rows)
	return
}

func (s *Service) executeJob(ctx context.Context, conn *pgxpool.Conn, j Job) error {
	if j.Mode == "normal" {
		return s.commitStep(ctx, conn, j, 0, nil, nil, "", "", false)
	}
	table := pgx.Identifier(strings.Split(j.table, ".")).Sanitize()
	if j.reversing() {
		return s.reverseBatches(ctx, conn, j, table)
	}
	return s.applyBatches(ctx, conn, j, table)
}

func (s *Service) applyBatches(ctx context.Context, conn *pgxpool.Conn, j Job, table string) error {
	if !j.execution.Valid {
		return errJobConflict
	}
	if err := initializeScope(ctx, conn, j.key, j.execution.Int64, table); err != nil {
		return err
	}
	for {
		var batch int64
		var cursor *int64
		err := conn.QueryRow(ctx, `SELECT batch,ids[array_upper(ids,1)] FROM wbd_migration_commits
          WHERE job_key=$1 AND NOT reversing ORDER BY batch DESC LIMIT 1`, j.key).Scan(&batch, &cursor)
		if err != nil {
			return err
		}
		if batch < 0 {
			batch = 0
		}
		rows, err := conn.Query(ctx, `SELECT id FROM wbd_migration_scope WHERE job_key=$1
            AND ($2::bigint IS NULL OR id>$2) ORDER BY id LIMIT $3`, j.key, cursor, j.size)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return s.commitStep(ctx, conn, j, 0, nil, nil, "", "", true)
		}
		if err = s.commitStep(ctx, conn, j, batch+1, ids, nil, table, "", false); err != nil {
			return err
		}
		if err = s.saveProgress(ctx, conn, j); err != nil {
			return err
		}
	}
}

func initializeScope(ctx context.Context, conn *pgxpool.Conn, key string, executionID int64, table string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO wbd_migration_commits(job_key,reversing,batch,execution_id)
      VALUES($1,false,-1,$2) ON CONFLICT DO NOTHING`, key, executionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	// Reject non-unique, nullable or non-BIGINT cursors. The supported batch
	// contract requires a single-column primary key, which also indexes scans.
	var valid bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute a JOIN pg_index i
        ON i.indrelid=a.attrelid AND i.indisprimary AND i.indnkeyatts=1 AND i.indkey[0]=a.attnum
        WHERE a.attrelid=to_regclass($1) AND a.attname='id' AND a.atttypid='bigint'::regtype)`, table).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return errJobInput
	}
	if _, err = tx.Exec(ctx, "INSERT INTO wbd_migration_scope(job_key,id) SELECT $1,id FROM "+table, key); err != nil {
		return err
	}
	return commitJobTransaction(ctx, tx)
}

func (s *Service) reverseBatches(ctx context.Context, conn *pgxpool.Conn, j Job, table string) error {
	sourceKey := j.key
	if j.Direction == "rollback" {
		if !j.source.Valid {
			return errJobConflict
		}
		if err := s.internalDB.QueryRowContext(ctx, "SELECT job_key FROM migration_jobs WHERE id=$1", j.source.Int64).Scan(&sourceKey); err != nil {
			return err
		}
	}
	for {
		var batch int64
		var ids []int64
		var before, after string
		err := conn.QueryRow(ctx, `SELECT a.batch,a.ids,a.before_rows::text,a.after_rows::text FROM wbd_migration_commits a
            WHERE a.job_key=$1 AND NOT a.reversing AND a.batch>0 AND NOT EXISTS(
                SELECT 1 FROM wbd_migration_commits r WHERE r.job_key=$2 AND r.reversing AND r.batch=a.batch)
            ORDER BY a.batch DESC LIMIT 1`, sourceKey, j.key).Scan(&batch, &ids, &before, &after)
		if errors.Is(err, pgx.ErrNoRows) {
			return s.commitStep(ctx, conn, j, 0, nil, nil, "", "", true)
		}
		if err != nil {
			return err
		}
		if err = s.commitStep(ctx, conn, j, batch, ids, &before, table, after, false); err != nil {
			return err
		}
		if err = s.saveProgress(ctx, conn, j); err != nil {
			return err
		}
	}
}

func (s *Service) saveProgress(ctx context.Context, conn *pgxpool.Conn, j Job) error {
	_, batches, rows, err := targetProgress(ctx, conn, j)
	if err != nil {
		return err
	}
	_, err = s.internalDB.ExecContext(ctx, "UPDATE migration_jobs SET batches_done=$2,rows_done=$3 WHERE id=$1", j.ID, batches, rows)
	return err
}

func (s *Service) commitStep(ctx context.Context, conn *pgxpool.Conn, j Job, batch int64, ids []int64,
	restore *string, table, expected string, markerOnly bool) (err error) {
	if !j.execution.Valid {
		return errJobConflict
	}
	started := time.Now()
	var affected int64
	defer func() {
		if s.observer == nil || markerOnly {
			return
		}
		status, committedRows := "success", affected
		if err != nil {
			status, committedRows = "failed", 0
			if errors.Is(err, errCommitUnknown) {
				status = "uncertain"
			}
		}
		s.observer.RecordBatch(observability.BatchResult{Status: status, Duration: time.Since(started), RowsAffected: committedRows})
	}()
	fields := j.fields()
	fields["batch"] = batch
	ctx, span := s.startTrace(ctx, "migration.batch", fields)
	defer func() { endTrace(span, traceJobError(err)) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr == nil {
			s.log(ctx, slog.LevelInfo, "migration.transaction.rollback", fields)
		}
	}()
	var before, after string
	if table != "" {
		before, err = readBatch(ctx, tx, table, ids)
		if err != nil {
			return err
		}
		if restore != nil && before != expected {
			return errCompensation
		}
	}
	if !markerOnly {
		var parameters []any
		if table != "" {
			parameters = []any{ids}
		}
		statements, parseErr := splitMigrationSQL(j.query)
		if parseErr != nil {
			return parseErr
		}
		// Match the lexical rules used to split scripts, even when the
		// target role has a different server default.
		if _, err = tx.Exec(ctx, "SET LOCAL standard_conforming_strings=on"); err != nil {
			return err
		}
		for index, statement := range statements {
			count, statementErr := s.runStatement(ctx, tx, j, batch, index, statement, parameters)
			if statementErr != nil {
				return statementErr
			}
			affected += count
		}
	}
	if table != "" {
		after, err = readBatch(ctx, tx, table, ids)
		if err != nil {
			return err
		}
		if restore != nil && after != *restore {
			return errCompensation
		}
		if affected != int64(len(ids)) {
			return errCompensation
		}
	}
	var cursor any
	if len(ids) > 0 {
		cursor = ids[len(ids)-1]
	}
	_, err = tx.Exec(ctx, `INSERT INTO wbd_migration_commits
      (job_key,reversing,batch,row_count,ids,before_rows,after_rows,execution_id,cursor_id)
      VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::jsonb,NULLIF($7,'')::jsonb,$8,$9)`,
		j.key, j.reversing(), batch, affected, ids, before, after, j.execution.Int64, cursor)
	if err != nil {
		return err
	}
	err = commitJobTransaction(ctx, tx)
	fields["status"] = "success"
	if err != nil {
		fields["status"] = "failed"
	}
	s.log(ctx, slog.LevelInfo, "migration.transaction.commit", fields)
	return err
}

func readBatch(ctx context.Context, tx pgx.Tx, table string, ids []int64) (string, error) {
	var count int64
	var snapshot string
	err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(jsonb_agg(to_jsonb(w) ORDER BY w.id),'[]')::text
        FROM (SELECT * FROM `+table+` WHERE id=ANY($1::bigint[]) FOR UPDATE) w`, ids).Scan(&count, &snapshot)
	if err == nil && count != int64(len(ids)) {
		err = errCompensation
	}
	return snapshot, err
}

func (s *Service) runStatement(ctx context.Context, tx pgx.Tx, j Job, batch int64, index int, statement migrationStatement, args []any) (int64, error) {
	fields := j.fields()
	fields["batch"], fields["query"], fields["statement"] = batch, statement.log, index+1
	started := time.Now()
	fields["parameters"] = []any{}
	if len(args) > 0 {
		fields["parameters"] = []string{"[batch IDs withheld]"}
	}
	s.log(ctx, slog.LevelInfo, "migration.query.started", fields)
	// Preparing through the extended protocol rejects multi-command strings.
	name := "wbd_migration_" + strconv.FormatInt(j.ID, 10) + "_" + strconv.Itoa(index)
	if _, err := tx.Conn().Prepare(ctx, name, statement.sql); err != nil {
		fields["status"], fields["sqlstate"] = "failed", sqlState(err)
		fields["duration_ms"] = time.Since(started).Milliseconds()
		s.log(ctx, slog.LevelError, "migration.query.finished", fields)
		return 0, err
	}
	tag, err := tx.Exec(ctx, name, args...)
	fields["status"], fields["rows"] = "success", tag.RowsAffected()
	fields["duration_ms"] = time.Since(started).Milliseconds()
	if err != nil {
		fields["status"], fields["sqlstate"] = "failed", sqlState(err)
	}
	s.log(ctx, slog.LevelInfo, "migration.query.finished", fields)
	if err != nil {
		return 0, fmt.Errorf("migration statement: %w", err)
	}
	return tag.RowsAffected(), nil
}
