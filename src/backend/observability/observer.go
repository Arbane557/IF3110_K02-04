package observability

import "time"

type Observer interface {
	RecordMigration(MigrationResult)
	RecordBatch(BatchResult)
	SetDatabasePool(active, idle int32)
}

type MigrationResult struct {
	Direction string
	Status    string
	Duration  time.Duration
}

type BatchResult struct {
	Status       string
	Duration     time.Duration
	RowsAffected int64
}
