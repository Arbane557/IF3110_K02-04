//go:build integration

package migration

import (
	"context"
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
	if err != nil || len(history) != 1 || history[0].Status != "success" {
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

func TestAsyncRabbitMQDelivery(t *testing.T) {
	url := os.Getenv("INTEGRATION_RABBITMQ_URL")
	if url == "" {
		t.Skip("set INTEGRATION_RABBITMQ_URL to a dedicated test broker")
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

func TestLargeDataBatchPerformance(t *testing.T) {
	if os.Getenv("RUN_MIGRATION_PERF") != "1" {
		t.Skip("set RUN_MIGRATION_PERF=1 to run the large data test")
	}
	s, target := asyncFixture(t)
	ctx := context.Background()
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
		if _, err := target.Pool.Exec(ctx, "UPDATE items SET processed=true"); err != nil {
			t.Fatal(err)
		}
		elapsed = time.Since(start)
		if elapsed > time.Second {
			break
		}
	}
	if elapsed <= time.Second {
		t.Fatalf("large UPDATE took %v; expected more than 1s", elapsed)
	}
	if _, err := target.Pool.Exec(ctx, "UPDATE items SET processed=false"); err != nil {
		t.Fatal(err)
	}
	m, err := s.Create(ctx, 1, 1, "UPDATE items SET processed=true WHERE id = ANY($1::bigint[])")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{"batch", "items", 10000})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err = s.processJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	job := requireJobStatus(t, s, j.ID, "success")
	if job.RowsDone != int64(count) {
		t.Fatalf("committed %d rows, expected %d", job.RowsDone, count)
	}
	t.Logf("rows=%d full_update=%s batched_update=%s batches=%d", count, elapsed, time.Since(start), job.BatchesDone)
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
