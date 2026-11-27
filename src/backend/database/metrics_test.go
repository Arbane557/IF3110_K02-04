package database

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	"wbd/backend/observability"
)

type queryLogRecorder struct {
	observability.Observer
	logs []observability.Fields
}

func (o *queryLogRecorder) Log(_ context.Context, _ slog.Level, _ string, fields observability.Fields) {
	o.logs = append(o.logs, fields)
}

func TestReadQueryTracerRedactsSQLParametersAndErrors(t *testing.T) {
	observer := &queryLogRecorder{Observer: observability.NewService(prometheus.NewRegistry(), trace.NewNoopTracerProvider().Tracer("test"))}
	tracer := poolAcquireTracer{observer: observer}
	ctx := WithReadOnlyQuery(context.Background(), 5, 7, "EXPLAIN [redacted SQL]")
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "EXPLAIN SELECT 'sensitive literal', $1", Args: []any{"sensitive parameter"},
	})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{
		Err: &pgconn.PgError{Code: "42601", Message: "sensitive message", Detail: "sensitive detail"},
	})
	if len(observer.logs) != 2 {
		t.Fatalf("got %d log events, want start and finish", len(observer.logs))
	}
	for _, fields := range observer.logs {
		if strings.Contains(fmt.Sprint(fields), "sensitive") {
			t.Fatal("query log exposed sensitive data")
		}
	}
	finished := observer.logs[1]
	if finished["query"] != "EXPLAIN [redacted SQL]" ||
		fmt.Sprint(finished["parameters"]) != "[[redacted]]" || finished["sqlstate"] != "42601" {
		t.Fatalf("missing safe query details: %+v", finished)
	}
}

func TestPoolAcquireTracerRecordsSuccessfulAndCancelledWaits(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer := observability.NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	tracer := poolAcquireTracer{observer: observer}
	for _, err := range []error{nil, context.Canceled} {
		ctx := context.WithValue(context.Background(), poolAcquireStartKey{}, time.Now().Add(-time.Second))
		tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: err})
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "database_pool_acquire_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			status := metric.Label[0].GetValue()
			seen[status] = true
			if metric.Histogram.GetSampleCount() != 1 || metric.Histogram.GetSampleSum() < 1 {
				t.Fatalf("%s pool wait was not recorded", status)
			}
		}
	}
	if !seen["success"] || !seen["failed"] {
		t.Fatal("success or cancellation metric missing")
	}
}

func TestPoolStatsDoesNotBlockOnConnectionLifecycle(t *testing.T) {
	manager := NewConnectionManager(nil, nil)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	done := make(chan struct{}, 1)
	go func() {
		manager.PoolStats()
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metrics scrape blocked behind a connection open/close")
	}
}
