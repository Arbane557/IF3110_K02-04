package observability

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

const (
	EventMigrationStarted  = "migration.started"
	EventMigrationFinished = "migration.finished"
	EventBatchStarted      = "migration.batch.started"
	EventBatchFinished     = "migration.batch.finished"
	EventCommit            = "migration.commit"
	EventRollback          = "migration.rollback"
)

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

func (s *Service) Log(ctx context.Context, level slog.Level, event string, fields Fields) {
	attributes := []slog.Attr{slog.String("event", event)}
	spanContext := trace.SpanContextFromContext(ctx)

	if spanContext.IsValid() {
		traceID := spanContext.TraceID().String()
		attributes = append(attributes,
			slog.String("correlation_id", traceID),
			slog.String("trace_id", traceID),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}

	for key, value := range fields {
		attributes = append(attributes, slog.Any(key, value))
	}

	s.logger.LogAttrs(ctx, level, event, attributes...)
}

