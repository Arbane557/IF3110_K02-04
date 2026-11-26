//go:build integration

package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"wbd/backend/auth"
	"wbd/backend/internal/testutil"
)

func asyncFixture(t *testing.T) (*Service, *testutil.Database) {
	t.Helper()
	platform := testutil.NewDatabase(t, true)
	target := testutil.NewDatabase(t, false)
	_, err := platform.SQL.Exec(`
        INSERT INTO users(id,username,password_hash,is_admin) VALUES
            (1,'actor','test',true),(2,'editor','test',false),(3,'viewer','test',false);
        INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext)
            VALUES(1,'target','localhost',5432,'test','test',decode('00','hex'));`)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(platform.SQL, integrationPool{target.Pool}), target
}

func requireJobStatus(t *testing.T, s *Service, id int64, status string) Job {
	t.Helper()
	j, err := s.GetJob(context.Background(), 1, id)
	if err != nil || j.Status != status {
		t.Fatalf("job=%+v, error=%v; want status %s", j, err, status)
	}
	return j
}

func TestTargetMetadataAddsExecutionAndCursorToExistingCheckpoints(t *testing.T) {
	ctx := context.Background()
	target := testutil.NewDatabase(t, false)
	if _, err := target.Pool.Exec(ctx, `CREATE TABLE wbd_migration_commits (
        job_key text NOT NULL,reversing boolean NOT NULL,batch bigint NOT NULL,row_count bigint NOT NULL DEFAULT 0,
        ids bigint[],before_rows jsonb,after_rows jsonb,PRIMARY KEY(job_key,reversing,batch))`); err != nil {
		t.Fatal(err)
	}
	conn, err := target.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err = ensureTargetMetadata(ctx, conn); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err = target.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
        WHERE table_schema=current_schema() AND table_name='wbd_migration_commits'
          AND column_name IN ('execution_id','cursor_id')`).Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("checkpoint columns=%d error=%v", columns, err)
	}
}

func TestAsyncMigrationApprovalAndCommitRecovery(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	if _, err := s.internalDB.Exec(`
        INSERT INTO groups(id,name,leader_user_id) VALUES (1,'Editors',2),(2,'Viewers',3);
        INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES (1,1,'edit'),(2,1,'view');`); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Pool.Exec(ctx, "CREATE TABLE items (id bigint PRIMARY KEY, value int); INSERT INTO items VALUES (1,0)"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(ctx, 1, 1, "SELECT 'quoted;value'; /* nested /* comment */ */ UPDATE items SET value=value+1")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(nil, s, jobTestAuthorizer{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/databases/{databaseID}/migrations/{version}/execute", handler.ExecuteMigration)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", fmt.Sprintf("/api/databases/1/migrations/%d/execute", m.Version), nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var j Job
	if err = json.Unmarshal(response.Body.Bytes(), &j); err != nil || j.Status != "pending_review" || j.ID <= 0 {
		t.Fatalf("job=%+v error=%v", j, err)
	}
	var before int
	if err = target.Pool.QueryRow(ctx, "SELECT value FROM items").Scan(&before); err != nil || before != 0 {
		t.Fatalf("submission ran target SQL: value=%d error=%v", before, err)
	}
	if _, err = s.ApproveJob(ctx, 1, j.ID, 1); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("non-leader approval: %v", err)
	}
	if err = s.processJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, s, j.ID, "pending_review")
	if j, err = s.ApproveJob(ctx, 1, j.ID, 2); err != nil || j.Status != "pending_review" {
		t.Fatalf("first approval: %+v %v", j, err)
	}
	if j, err = s.ApproveJob(ctx, 1, j.ID, 3); err != nil || j.Status != "pending" {
		t.Fatalf("final approval: %+v %v", j, err)
	}

	// Simulate a crash after SQL and proof commit but before platform history is finalized.
	j, err = s.startAttempt(ctx, j)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := target.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if err = ensureTargetMetadata(ctx, conn); err != nil {
		t.Fatal(err)
	}
	if err = s.executeJob(ctx, conn, j); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.processJob(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	requireJobStatus(t, s, j.ID, "success")
	var value int
	if err = target.Pool.QueryRow(ctx, "SELECT value FROM items").Scan(&value); err != nil || value != 1 {
		t.Fatalf("value=%d err=%v", value, err)
	}
	history, err := s.ListHistory(ctx, 1, false)
	if err != nil || len(history) != 1 || history[0].Status != "success" || history[0].SQL != m.SQL || history[0].UserID != 1 || history[0].ExecutionID <= 0 || history[0].StartedAt.IsZero() || history[0].ExecutedAt == nil {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestAsyncBatchResumeAndCompensation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			ctx := context.Background()
			s, target := asyncFixture(t)
			if _, err := target.Pool.Exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY,value int DEFAULT 0,
                CONSTRAINT fail_batch CHECK(id<3 OR value=0)); INSERT INTO items(id) VALUES (1),(2),(3),(4)`); err != nil {
				t.Fatal(err)
			}
			m, err := s.CreateWithDown(ctx, 1, 1, "UPDATE items SET value=value+1 WHERE id = ANY($1::bigint[])",
				"UPDATE items SET value=value-1 WHERE id = ANY($1::bigint[])", false, "")
			if err != nil {
				t.Fatal(err)
			}
			j, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{"batch", "items", 2})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.processJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			partial := requireJobStatus(t, s, j.ID, "partial")
			if partial.BatchesDone != 1 || partial.RowsDone != 2 {
				t.Fatalf("progress=%+v", partial)
			}
			var committed, untouched int
			if err := target.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE id<=2 AND value=1),
                count(*) FILTER (WHERE id>2 AND value=0) FROM items`).Scan(&committed, &untouched); err != nil || committed != 2 || untouched != 2 {
				t.Fatalf("partial state: committed=%d untouched=%d error=%v", committed, untouched, err)
			}
			if _, err = target.Pool.Exec(ctx, "ALTER TABLE items DROP CONSTRAINT fail_batch"); err != nil {
				t.Fatal(err)
			}
			if _, err = s.RecoverJob(ctx, 1, j.ID, 1, cancel); err != nil {
				t.Fatal(err)
			}
			if err = s.processJob(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			requireJobStatus(t, s, j.ID, "success")
			var sum, max int
			if err = target.Pool.QueryRow(ctx, "SELECT sum(value),max(value) FROM items").Scan(&sum, &max); err != nil {
				t.Fatal(err)
			}
			if cancel && sum != 0 || !cancel && (sum != 4 || max != 1) {
				t.Fatalf("sum=%d max=%d", sum, max)
			}
			if !cancel {
				for _, direction := range []string{"rollback", "forward"} {
					next, err := s.SubmitJob(ctx, 1, m.Version, 1, direction, BatchOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if err = s.processJob(ctx, next.ID); err != nil {
						t.Fatal(err)
					}
					requireJobStatus(t, s, next.ID, "success")
				}
			}
		})
	}
}

func TestAsyncBatchCompensationResumesFromCommittedCheckpoint(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	if _, err := target.Pool.Exec(ctx, `CREATE TABLE items (id bigint PRIMARY KEY,value int DEFAULT 0,
        CONSTRAINT fail_batch CHECK(id<5 OR value=0)); INSERT INTO items(id) SELECT generate_series(1,6)`); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateWithDown(ctx, 1, 1, "UPDATE items SET value=value+1 WHERE id = ANY($1::bigint[])",
		"UPDATE items SET value=value-1 WHERE id = ANY($1::bigint[])", false, "")
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{"batch", "items", 2})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.processJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	partial := requireJobStatus(t, s, job.ID, "partial")
	if partial.BatchesDone != 2 || partial.RowsDone != 4 || !partial.execution.Valid {
		t.Fatalf("partial checkpoint=%+v", partial)
	}
	originalExecution := partial.execution.Int64

	var jobKey string
	if err = s.internalDB.QueryRowContext(ctx, "SELECT job_key FROM migration_jobs WHERE id=$1", job.ID).Scan(&jobKey); err != nil {
		t.Fatal(err)
	}
	var checkpoints, complete int
	if err = target.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE execution_id>0 AND cursor_id IS NOT NULL)
        FROM wbd_migration_commits WHERE job_key=$1 AND NOT reversing AND batch>0`, jobKey).Scan(&checkpoints, &complete); err != nil || checkpoints != 2 || complete != 2 {
		t.Fatalf("batch checkpoints=%d complete=%d error=%v", checkpoints, complete, err)
	}
	var originalCursors []int64
	if err = target.Pool.QueryRow(ctx, `SELECT array_agg(cursor_id ORDER BY batch)
        FROM wbd_migration_commits WHERE job_key=$1 AND NOT reversing AND batch>0`, jobKey).Scan(&originalCursors); err != nil || len(originalCursors) != 2 || originalCursors[0] != 2 || originalCursors[1] != 4 {
		t.Fatalf("original batch cursors=%v error=%v", originalCursors, err)
	}

	if _, err = target.Pool.Exec(ctx, `ALTER TABLE items DROP CONSTRAINT fail_batch;
        CREATE FUNCTION reject_middle_compensation() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN
            -- Batch 2 (IDs 3–4) must commit before batch 1 is blocked.
            IF OLD.id BETWEEN 1 AND 2 AND NEW.value=0 THEN RAISE EXCEPTION 'blocked compensation'; END IF;
            RETURN NEW;
        END $$;
        CREATE TRIGGER reject_middle_compensation BEFORE UPDATE ON items
        FOR EACH ROW EXECUTE FUNCTION reject_middle_compensation()`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverJob(ctx, 1, job.ID, 1, true); err != nil {
		t.Fatal(err)
	}
	if err = s.processJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	partiallyReverted := requireJobStatus(t, s, job.ID, "partially_reverted")
	if partiallyReverted.BatchesDone != 1 || partiallyReverted.RowsDone != 2 || !partiallyReverted.execution.Valid {
		t.Fatalf("compensation progress=%+v", partiallyReverted)
	}
	var firstCompensatedBatch int64
	if err = target.Pool.QueryRow(ctx, `SELECT batch FROM wbd_migration_commits
        WHERE job_key=$1 AND reversing AND batch>0`, jobKey).Scan(&firstCompensatedBatch); err != nil || firstCompensatedBatch != 2 {
		t.Fatalf("first compensated batch=%d error=%v; expected reverse order from batch 2", firstCompensatedBatch, err)
	}
	if _, err = target.Pool.Exec(ctx, "DROP TRIGGER reject_middle_compensation ON items; DROP FUNCTION reject_middle_compensation()"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecoverJob(ctx, 1, job.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	if err = s.processJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	completed := requireJobStatus(t, s, job.ID, "success")
	if completed.BatchesDone != 2 || completed.RowsDone != 4 {
		t.Fatalf("completed compensation=%+v", completed)
	}
	var untouched int
	if err = target.Pool.QueryRow(ctx, "SELECT count(*) FROM items WHERE value<>0").Scan(&untouched); err != nil || untouched != 0 {
		t.Fatalf("compensation left %d changed rows: %v", untouched, err)
	}
	var linkedExecution sql.NullInt64
	if err = s.internalDB.QueryRowContext(ctx, `SELECT cancelled_execution_id FROM migration_history
        WHERE database_id=1 AND version=$1 AND direction='rollback' ORDER BY execution_id DESC LIMIT 1`, m.Version).Scan(&linkedExecution); err != nil || !linkedExecution.Valid || linkedExecution.Int64 != originalExecution {
		t.Fatalf("rollback history linked to %v, want execution %d (error %v)", linkedExecution, originalExecution, err)
	}
	if err = target.Pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER(WHERE execution_id>0 AND cursor_id IS NOT NULL)
        FROM wbd_migration_commits WHERE job_key=$1 AND batch>0`, jobKey).Scan(&checkpoints, &complete); err != nil || checkpoints != 4 || complete != 4 {
		t.Fatalf("final checkpoints=%d complete=%d error=%v", checkpoints, complete, err)
	}
	var compensationBatches, compensationCursors []int64
	if err = target.Pool.QueryRow(ctx, `SELECT array_agg(batch ORDER BY execution_id),array_agg(cursor_id ORDER BY execution_id)
        FROM wbd_migration_commits WHERE job_key=$1 AND reversing AND batch>0`, jobKey).Scan(&compensationBatches, &compensationCursors); err != nil ||
		len(compensationBatches) != 2 || compensationBatches[0] != 2 || compensationBatches[1] != 1 ||
		len(compensationCursors) != 2 || compensationCursors[0] != 4 || compensationCursors[1] != 2 {
		t.Fatalf("compensation checkpoints=%v cursors=%v error=%v", compensationBatches, compensationCursors, err)
	}
}

func TestAsyncRabbitMQDelivery(t *testing.T) {
	url := os.Getenv("INTEGRATION_RABBITMQ_URL")
	if url == "" {
		t.Fatal("integration tests require INTEGRATION_RABBITMQ_URL pointing to a dedicated test broker")
	}
	s, target := asyncFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m, err := s.Create(ctx, 1, 1, "CREATE TABLE from_worker (id bigint PRIMARY KEY)")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.RunWorker(ctx, url, 2) }()
	defer func() { cancel(); <-done }()
	for ctx.Err() == nil {
		job, err := s.GetJob(ctx, 1, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "success" {
			if _, err = target.Pool.Exec(ctx, "SELECT id FROM from_worker"); err != nil {
				t.Fatal(err)
			}
			return
		}
		if !jobRunnable(job) {
			t.Fatalf("job=%+v", job)
		}
		waitWorker(ctx, 100*time.Millisecond)
	}
	t.Fatal("worker did not complete the RabbitMQ job")
}

func TestAsyncRollbackForwardHistoryIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	if _, err := target.Pool.Exec(ctx, "CREATE TABLE items(id bigint PRIMARY KEY, value int); INSERT INTO items VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateWithDown(ctx, 1, 1, "UPDATE items SET value=value+1", "UPDATE items SET value=value-1", false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"up", "rollback", "forward"} {
		job, err := s.SubmitJob(ctx, 1, m.Version, 1, direction, BatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.processJob(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		requireJobStatus(t, s, job.ID, "success")
		if _, err := s.Update(ctx, 1, m.Version, "UPDATE items SET value=99"); !errors.Is(err, ErrMigrationLocked) {
			t.Fatalf("immutable SQL after %s: %v", direction, err)
		}
	}
	var value int
	if err := target.Pool.QueryRow(ctx, "SELECT value FROM items WHERE id=1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("forward state=%d error=%v", value, err)
	}
	history, err := s.ListHistory(ctx, 1, false)
	if err != nil || len(history) != 3 {
		t.Fatalf("history=%+v error=%v", history, err)
	}
	forward, rollback, up := history[0], history[1], history[2]
	if forward.Direction != "forward" || rollback.Direction != "rollback" || up.Direction != "up" ||
		forward.SQL != m.SQL || rollback.SQL != m.SQLDown || up.SQL != m.SQL ||
		rollback.CancelledExecutionID == nil || *rollback.CancelledExecutionID != up.ExecutionID ||
		forward.ExecutionID == up.ExecutionID {
		t.Fatalf("execution snapshots=%+v", history)
	}
	visible, err := s.ListHistory(ctx, 1, true)
	if err != nil || len(visible) != 1 || visible[0].ExecutionID != forward.ExecutionID {
		t.Fatalf("filtered history=%+v error=%v", visible, err)
	}
	all, err := s.ListHistory(ctx, 1, false)
	if err != nil || len(all) != 3 {
		t.Fatalf("filter changed stored history=%+v error=%v", all, err)
	}
}

func TestAsyncFailedRollbackPreservesActiveStateAndHistoryIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	if _, err := target.Pool.Exec(ctx, "CREATE TABLE items(id bigint PRIMARY KEY, value int); INSERT INTO items VALUES(1,0)"); err != nil {
		t.Fatal(err)
	}
	m, err := s.CreateWithDown(ctx, 1, 1, "UPDATE items SET value=1",
		"UPDATE items SET value=0; SELECT * FROM missing_relation", false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"up", "rollback"} {
		job, err := s.SubmitJob(ctx, 1, m.Version, 1, direction, BatchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.processJob(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		status := "success"
		if direction == "rollback" {
			status = "failed"
		}
		requireJobStatus(t, s, job.ID, status)
	}
	var value int
	if err := target.Pool.QueryRow(ctx, "SELECT value FROM items WHERE id=1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("failed down committed changes: value=%d error=%v", value, err)
	}
	persisted, err := s.Get(ctx, 1, m.Version)
	if err != nil || !persisted.Active || persisted.Status != "success" {
		t.Fatalf("failed rollback changed active migration=%+v error=%v", persisted, err)
	}
	for _, hide := range []bool{false, true} {
		history, err := s.ListHistory(ctx, 1, hide)
		if err != nil || len(history) != 2 || history[0].Direction != "rollback" || history[0].Status != "failed" ||
			history[0].SQL != m.SQLDown || history[0].ErrorMessage == "" || history[0].ExecutedAt == nil || history[1].Status != "success" {
			t.Fatalf("hide=%v history=%+v error=%v", hide, history, err)
		}
	}
}

func TestLargeDataBatchPerformance(t *testing.T) {
	if os.Getenv("RUN_MIGRATION_PERF") != "1" {
		t.Skip("set RUN_MIGRATION_PERF=1 to run the large data test")
	}
	s, target := asyncFixture(t)
	ctx := context.Background()
	var postgresVersion string
	if err := target.Pool.QueryRow(ctx, "SHOW server_version_num").Scan(&postgresVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Pool.Exec(ctx, "CREATE TABLE items (id bigint PRIMARY KEY, processed bool DEFAULT false)"); err != nil {
		t.Fatal(err)
	}
	var count int
	var elapsed time.Duration
	for count = 125000; count <= 8000000; count *= 2 {
		if _, err := target.Pool.Exec(ctx, "TRUNCATE items"); err != nil {
			t.Fatal(err)
		}
		if _, err := target.Pool.Exec(ctx, "INSERT INTO items(id) SELECT generate_series(1,$1)", count); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		tag, err := target.Pool.Exec(ctx, "UPDATE items SET processed=true")
		elapsed = time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != int64(count) {
			t.Fatalf("full UPDATE affected %d rows, expected %d", tag.RowsAffected(), count)
		}
		if elapsed > time.Second {
			break
		}
	}
	if elapsed <= time.Second {
		t.Fatalf("large UPDATE took %v; expected more than 1s", elapsed)
	}
	tag, err := target.Pool.Exec(ctx, "UPDATE items SET processed=false WHERE processed")
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != int64(count) {
		t.Fatalf("reset affected %d rows, expected %d", tag.RowsAffected(), count)
	}
	const batchSize = 10000
	m, err := s.Create(ctx, 1, 1, "UPDATE items SET processed=true WHERE id = ANY($1::bigint[])")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{"batch", "items", batchSize})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = s.processJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	batchedElapsed := time.Since(start)
	job := requireJobStatus(t, s, j.ID, "success")
	if job.RowsDone != int64(count) {
		t.Fatalf("committed %d rows, expected %d", job.RowsDone, count)
	}
	if want := int64((count + batchSize - 1) / batchSize); job.BatchesDone != want {
		t.Fatalf("committed %d batches, expected %d", job.BatchesDone, want)
	}
	var processed int64
	if err := target.Pool.QueryRow(ctx, "SELECT count(*) FROM items WHERE processed").Scan(&processed); err != nil || processed != int64(count) {
		t.Fatalf("persisted processed rows=%d expected=%d error=%v", processed, count, err)
	}
	t.Logf("postgres_version_num=%s rows=%d full_update=%s batched_update=%s batch_size=%d batches=%d",
		postgresVersion, count, elapsed, batchedElapsed, batchSize, job.BatchesDone)
}

type jobTestAuthorizer struct{}

func (jobTestAuthorizer) PrincipalFromRequest(*http.Request) (auth.Principal, error) {
	return auth.Principal{UserID: 1, IsPlatformAdmin: true}, nil
}

func (jobTestAuthorizer) AuthorizeDatabase(context.Context, auth.Principal, int64, string) error {
	return nil
}

func TestAsyncDatabaseLockAndQueueOrder(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	first, err := s.Create(ctx, 1, 1, "CREATE TABLE ordered (id bigint PRIMARY KEY)")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Create(ctx, 1, 1, "INSERT INTO ordered VALUES(1)")
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.SubmitJob(ctx, 1, first.Version, 1, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.SubmitJob(ctx, 1, second.Version, 1, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := target.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_lock("+targetLock+")"); err != nil {
		t.Fatal(err)
	}
	if err = s.processJob(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, s, one.ID, "pending")
	if _, err = conn.Exec(ctx, "SELECT pg_advisory_unlock("+targetLock+")"); err != nil {
		t.Fatal(err)
	}
	if err = s.processJob(ctx, two.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, s, two.ID, "pending")
	for _, id := range []int64{one.ID, two.ID} {
		if err = s.processJob(ctx, id); err != nil {
			t.Fatal(err)
		}
		requireJobStatus(t, s, id, "success")
	}
}

func TestAsyncWorkerRevalidatesActorAccessIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	if _, err := s.internalDB.Exec(`
        INSERT INTO groups(id,name,leader_user_id) VALUES(1,'Editors',2);
        INSERT INTO group_members(group_id,user_id) VALUES(1,2);
        INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,1,'edit')`); err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(ctx, 1, 2, "CREATE TABLE forbidden_change(id bigint)")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.SubmitJob(ctx, 1, m.Version, 2, "up", BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveJob(ctx, 1, j.ID, 2); err != nil {
		t.Fatal(err)
	}
	// Approval cannot preserve execution rights after access is downgraded.
	if _, err := s.internalDB.Exec("UPDATE group_database_permissions SET access_level='view' WHERE group_id=1 AND database_id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.processJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	requireJobStatus(t, s, j.ID, "failed")
	var exists bool
	if err := target.Pool.QueryRow(ctx, "SELECT to_regclass('forbidden_change') IS NOT NULL").Scan(&exists); err != nil || exists {
		t.Fatalf("revoked actor changed target: exists=%v error=%v", exists, err)
	}
	history, err := s.ListHistory(ctx, 1, false)
	if err != nil || len(history) != 1 || history[0].Status != "failed" || history[0].UserID != 2 {
		t.Fatalf("denied attempt history=%+v error=%v", history, err)
	}
}
