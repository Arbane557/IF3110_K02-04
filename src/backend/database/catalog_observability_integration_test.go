//go:build integration

package database

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"wbd/backend/internal/testutil"
	"wbd/backend/observability"
)

type catalogLog struct {
	event  string
	span   trace.SpanContext
	fields observability.Fields
}

type catalogObserver struct {
	observability.Observer
	logs []catalogLog
}

func (o *catalogObserver) Log(ctx context.Context, _ slog.Level, event string, fields observability.Fields) {
	o.logs = append(o.logs, catalogLog{event, trace.SpanContextFromContext(ctx), fields})
}

func TestCatalogQueryObservability(t *testing.T) {
	fixture := testutil.NewDatabase(t, false)
	ctx := context.Background()
	if _, err := fixture.Pool.Exec(ctx, `
		CREATE TABLE private_table (id int PRIMARY KEY);
		INSERT INTO private_table VALUES (1), (2);
		CREATE VIEW broken_view AS SELECT 'private-data'::text AS data, 1 / 0 AS broken;
	`); err != nil {
		t.Fatal(err)
	}
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	observer := &catalogObserver{Observer: observability.NewService(prometheus.NewRegistry(), provider.Tracer("test"))}
	config := fixture.Pool.Config()
	config.ConnConfig.Tracer = poolAcquireTracer{observer: observer}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	service := NewCatalogService(integrationPoolProvider{pool})
	ctx, parent := provider.Tracer("test").Start(ctx, "http")
	defer parent.End()
	ctx = WithReadOnlyQuery(ctx, 5, 7, "")

	if _, err = service.GetTableDetails(ctx, 5, fixture.Schema, "private_table"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetRows(ctx, 5, fixture.Schema, "private_table", Page{Number: 1, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.GetRows(ctx, 5, fixture.Schema, "broken_view", Page{Number: 1, Size: 2}); err == nil {
		t.Fatal("expected a division-by-zero query error")
	}

	// Four catalog queries for table details and two per data request.
	spans := recorder.Ended()
	if len(spans) != 8 || len(observer.logs) != 16 {
		t.Fatalf("got %d spans and %d logs", len(spans), len(observer.logs))
	}
	for i, span := range spans {
		started, finished := observer.logs[2*i], observer.logs[2*i+1]
		if span.Name() != "database.query" || !span.Parent().Equal(parent.SpanContext()) ||
			!started.span.Equal(span.SpanContext()) || !finished.span.Equal(span.SpanContext()) ||
			started.event != "database.query.started" || finished.event != "database.query.finished" ||
			started.fields["status"] != "running" || finished.fields["actor"] != int64(7) ||
			finished.fields["database"] != int64(5) || finished.fields["duration_ms"] == nil {
			t.Fatalf("unexpected query telemetry for span %d: %+v %+v", i, started, finished)
		}
		if logged := fmt.Sprint(finished.fields); strings.Contains(logged, "private_table") ||
			strings.Contains(logged, "broken_view") || strings.Contains(logged, "private-data") {
			t.Fatalf("query log exposed an identifier or data: %s", logged)
		}
	}
	if fields := observer.logs[11].fields; fields["rows"] != int64(2) || fields["status"] != "success" ||
		fmt.Sprint(fields["parameters"]) != "[2 0]" {
		t.Fatalf("unexpected data query result: %+v", fields)
	}
	if fields := observer.logs[15].fields; fields["sqlstate"] != "22012" ||
		fields["status"] != "failed" || spans[7].Status().Code != codes.Error ||
		spans[7].Status().Description != "database query failed (SQLSTATE 22012)" {
		t.Fatalf("unexpected failed query result: %+v", fields)
	}
}
