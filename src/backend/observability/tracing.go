package observability

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	SpanHTTP                     = "wbd.http"
	SpanMigrationExecute         = "migration.execute"
	SpanMigrationStartExecution  = "migration.internal.start_execution"
	SpanMigrationAcquireTarget   = "migration.target.acquire"
	SpanMigrationBeginTarget     = "migration.target.begin"
	SpanMigrationLockTarget      = "migration.target.lock"
	SpanMigrationBatch           = "migration.batch"
	SpanMigrationCommitTarget    = "migration.target.commit"
	SpanMigrationRollbackTarget  = "migration.target.rollback"
	SpanMigrationMarkSuccess     = "migration.internal.mark_success"
	SpanMigrationMarkFailed      = "migration.internal.mark_failed"
	EventMigrationBatchResult    = "migration.batch.result"
)

func NewTracerProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "wbd-backend"
	}
	resources, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("create OpenTelemetry resource: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resources),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider, nil
}

type traceSpan struct {
	span trace.Span
}

func (s *Service) StartTrace(ctx context.Context, name string, fields Fields) (context.Context, Span) {
	ctx, span := s.tracer.Start(ctx, name, trace.WithAttributes(fieldAttributes(fields)...))
	return ctx, traceSpan{span: span}
}

func (s traceSpan) AddEvent(name string, fields Fields) {
	s.span.AddEvent(name, trace.WithAttributes(fieldAttributes(fields)...))
}

func (s traceSpan) End(err error) {
	if err != nil {
		s.span.RecordError(err)
		s.span.SetStatus(codes.Error, err.Error())
	} else {
		s.span.SetStatus(codes.Ok, "")
	}
	s.span.End()
}

func fieldAttributes(fields Fields) []attribute.KeyValue {
	attributes := make([]attribute.KeyValue, 0, len(fields))
	for key, value := range fields {
		switch value := value.(type) {
		case string:
			attributes = append(attributes, attribute.String(key, value))
		case bool:
			attributes = append(attributes, attribute.Bool(key, value))
		case int:
			attributes = append(attributes, attribute.Int(key, value))
		case int32:
			attributes = append(attributes, attribute.Int64(key, int64(value)))
		case int64:
			attributes = append(attributes, attribute.Int64(key, value))
		case float64:
			attributes = append(attributes, attribute.Float64(key, value))
		case time.Duration:
			attributes = append(attributes, attribute.Int64(key, value.Milliseconds()))
		default:
			attributes = append(attributes, attribute.String(key, fmt.Sprint(value)))
		}
	}
	return attributes
}
