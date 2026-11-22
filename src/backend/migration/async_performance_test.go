//go:build integration

package migration

import (
	"context"
	"os"
	"testing"
	"time"

	"wbd/backend/internal/testutil"
)

// Run explicitly against a disposable PostgreSQL database. It grows the same
// fixture until a single unbatched UPDATE takes longer than one second.
func TestLargeDataBatchPerformance(t *testing.T) {
	if os.Getenv("RUN_MIGRATION_PERF") != "1" {
		t.Skip("set RUN_MIGRATION_PERF=1 for the large data measurement")
	}
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO users(id,username,password_hash) VALUES(7,'perf-leader','unused');
 INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext) VALUES(1,'perf','localhost',5432,'perf','test','\x00');
 INSERT INTO groups(id,name,leader_user_id) VALUES(1,'perf-team',7);
 INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,1,'edit')`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.Pool.Exec(ctx, `CREATE TABLE perf_items(id BIGINT PRIMARY KEY, value INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL DEFAULT repeat('x',128))`); err != nil {
		t.Fatal(err)
	}
	count := 100000
	var baseline time.Duration
	for count <= 1600000 {
		if _, err := fixture.Pool.Exec(ctx, `TRUNCATE perf_items; INSERT INTO perf_items(id) SELECT generate_series(1,$1::bigint)`, count); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if _, err := fixture.Pool.Exec(ctx, `UPDATE perf_items SET value=value+1`); err != nil {
			t.Fatal(err)
		}
		baseline = time.Since(started)
		if baseline > time.Second {
			break
		}
		count *= 2
	}
	if baseline <= time.Second {
		t.Fatalf("unbatched UPDATE did not exceed 1s with %d rows; increase fixture limit", count)
	}
	t.Logf("unbatched: rows=%d duration=%s", count, baseline)
	if _, err := fixture.Pool.Exec(ctx, `UPDATE perf_items SET value=0`); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	item, err := service.Create(ctx, 1, 7, `UPDATE perf_items SET value=value+1 WHERE id = ANY($1::bigint[])`)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, 1, item.Version, 7, "up", "batch", "perf_items", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ApproveJob(ctx, 1, job.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	service.runJob(ctx, claimed)
	batchDuration := time.Since(started)
	result, err := service.GetJob(ctx, 1, job.ID)
	if err != nil || result.Status != "success" || result.RowsDone != int64(count) {
		t.Fatalf("job=%+v error=%v", result, err)
	}
	t.Logf("batch: rows=%d size=1000 batches=%d duration=%s", count, result.BatchesDone, batchDuration)
}
