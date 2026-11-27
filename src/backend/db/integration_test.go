//go:build integration

package db

import (
	"context"
	"testing"

	"wbd/backend/internal/testutil"
)

func TestInstallMigrationMetricsUpgradesExistingJobsIdempotently(t *testing.T) {
	fixture := testutil.NewDatabase(t, true)
	_, err := fixture.SQL.Exec(`
        INSERT INTO users(id,username,password_hash) VALUES(1,'actor','unused');
        INSERT INTO managed_databases(id,name,host,port,database_name,username,password_ciphertext)
          VALUES(1,'target','localhost',5432,'test','test',decode('00','hex'));
        INSERT INTO migrations(database_id,version,sql_text,created_by,status) VALUES(1,1,'SELECT 1',1,'draft');
        INSERT INTO migration_jobs(database_id,version,actor,direction,mode,status,sql_text,sql_down,batch_table,batch_size)
          VALUES(1,1,1,'up','normal','pending','SELECT 1','','',0);
		ALTER TABLE migration_jobs DROP COLUMN queued_at, DROP COLUMN lock_wait_started_at, DROP COLUMN cancelled_execution_id`)
	if err != nil { t.Fatal(err) }
	for i := 0; i < 2; i++ {
		if err := InstallMigrationJobs(context.Background(), fixture.SQL); err != nil { t.Fatal(err) }
	}
	var count int
	if err := fixture.SQL.QueryRow(`SELECT count(*) FROM migration_jobs j
	    WHERE status='pending' AND sql_text='SELECT 1' AND queued_at IS NOT NULL AND lock_wait_started_at IS NULL
	      AND EXISTS(SELECT 1 FROM information_schema.columns c WHERE c.table_schema=current_schema()
	        AND c.table_name='migration_jobs' AND c.column_name='cancelled_execution_id')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("upgraded jobs=%d error=%v", count, err)
	}
}
