//go:build integration

package migration

import (
	"context"
	"testing"

	"wbd/backend/internal/testutil"
)

func TestAsyncBatchCheckpointAndDuplicateDelivery(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO users(id,username,password_hash) VALUES(7,'worker-test','unused')`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext) VALUES(1,'test','localhost',5432,'test','test','\x00')`); err != nil {
		t.Fatal(err)
	}
	seedApprovalGroup(t, fixture)
	if _, err := fixture.Pool.Exec(ctx, `CREATE TABLE items(id BIGINT PRIMARY KEY, value INTEGER NOT NULL DEFAULT 0); INSERT INTO items(id) SELECT generate_series(1,7)`); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	migration, err := service.Create(ctx, 1, 7, `UPDATE items SET value=value+1 WHERE id = ANY($1::bigint[])`)
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, 1, migration.Version, 7, "up", "batch", "items", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveJob(ctx, 1, job.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.claim(ctx)
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim=%+v error=%v", claimed, err)
	}
	service.runJob(ctx, claimed)
	actual, err := service.GetJob(ctx, 1, job.ID)
	if err != nil || actual.Status != "success" || actual.BatchesDone != 3 || actual.RowsDone != 7 {
		t.Fatalf("job=%+v error=%v", actual, err)
	}
	service.runJob(ctx, claimed) // duplicate queue delivery cannot execute the UPDATE twice
	var count, minimum int
	if err := fixture.Pool.QueryRow(ctx, `SELECT COUNT(*),MIN(value) FROM items`).Scan(&count, &minimum); err != nil {
		t.Fatal(err)
	}
	if count != 7 || minimum != 1 {
		t.Fatalf("rows=%d minimum=%d", count, minimum)
	}
}

func TestAsyncBatchRollbackCompensatesInReverse(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO users(id,username,password_hash) VALUES(7,'batch-rollback-test','unused')`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext) VALUES(1,'test','localhost',5432,'test','test','\x00')`); err != nil {
		t.Fatal(err)
	}
	seedApprovalGroup(t, fixture)
	if _, err := fixture.Pool.Exec(ctx, `CREATE TABLE items(id BIGINT PRIMARY KEY,value INTEGER NOT NULL DEFAULT 0); INSERT INTO items(id) SELECT generate_series(1,5)`); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	migration, err := service.CreateWithDown(ctx, 1, 7, `UPDATE items SET value=value+1 WHERE id = ANY($1::bigint[])`, `UPDATE items SET value=value-1 WHERE id = ANY($1::bigint[])`, false, "")
	if err != nil {
		t.Fatal(err)
	}
	up, err := service.Enqueue(ctx, 1, migration.Version, 7, "up", "batch", "items", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveJob(ctx, 1, up.ID, 7); err != nil {
		t.Fatal(err)
	}
	claimed, err := service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service.runJob(ctx, claimed)
	rollback, err := service.Enqueue(ctx, 1, migration.Version, 7, "rollback", "normal", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ApproveJob(ctx, 1, rollback.ID, 7); err != nil {
		t.Fatal(err)
	}
	if rollback.Mode != "batch" || rollback.SourceJobID == nil || *rollback.SourceJobID != up.ID {
		t.Fatalf("rollback job=%+v", rollback)
	}
	claimed, err = service.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service.runJob(ctx, claimed)
	actual, err := service.GetJob(ctx, 1, rollback.ID)
	if err != nil || actual.Status != "success" || actual.BatchesDone != 3 {
		t.Fatalf("rollback=%+v error=%v", actual, err)
	}
	var sum int
	if err := fixture.Pool.QueryRow(ctx, `SELECT SUM(value) FROM items`).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Fatalf("rollback left value sum=%d", sum)
	}
}

func seedApprovalGroup(t *testing.T, fixture *testutil.Database) {
	t.Helper()
	if _, err := fixture.SQL.Exec(`INSERT INTO groups(id,name,leader_user_id) VALUES(1,'team',7);
 INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,1,'edit')`); err != nil {
		t.Fatal(err)
	}
}

func TestAllGroupLeadersMustApprove(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	ctx := context.Background()
	if _, err := fixture.SQL.ExecContext(ctx, `INSERT INTO users(id,username,password_hash) VALUES(7,'leader-one','unused'),(8,'leader-two','unused');
 INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext) VALUES(1,'target','localhost',5432,'target','test','\x00');
 INSERT INTO groups(id,name,leader_user_id) VALUES(1,'one',7),(2,'two',8);
 INSERT INTO group_database_permissions(group_id,database_id,access_level) VALUES(1,1,'edit'),(2,1,'view')`); err != nil {
		t.Fatal(err)
	}
	service := NewService(fixture.SQL, integrationPool{fixture.Pool})
	item, err := service.Create(ctx, 1, 7, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	job, err := service.Enqueue(ctx, 1, item.Version, 7, "up", "normal", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "pending_review" {
		t.Fatalf("status=%s", job.Status)
	}
	if _, err = service.ApproveJob(ctx, 1, job.ID, 7); err != nil {
		t.Fatal(err)
	}
	current, err := service.GetJob(ctx, 1, job.ID)
	if err != nil || current.Status != "pending_review" {
		t.Fatalf("first approval=%+v %v", current, err)
	}
	if _, err = service.ApproveJob(ctx, 1, job.ID, 8); err != nil {
		t.Fatal(err)
	}
	current, err = service.GetJob(ctx, 1, job.ID)
	if err != nil || current.Status != "pending" {
		t.Fatalf("second approval=%+v %v", current, err)
	}
}
