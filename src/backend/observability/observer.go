package observability

import (
	"context"
	"log/slog"
	"time"
)

type Fields map[string]any

type Span interface {
	AddEvent(name string, fields Fields)
	End(err error)
}

type Tracer interface {
	StartTrace(ctx context.Context, name string, fields Fields) (context.Context, Span)
}

type MetricsRecorder interface {
	RecordMigration(MigrationResult)
	RecordBatch(BatchResult)
	SetDatabasePool(active, idle int32)
}

type Logger interface {
	Log(ctx context.Context, level slog.Level, event string, fields Fields)
}

type Observer interface {
	Tracer
	MetricsRecorder
	Logger
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
