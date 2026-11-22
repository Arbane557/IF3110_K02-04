package database

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
	"wbd/backend/observability"
)

func TestPoolAcquireTracerRecordsSuccessfulAndCancelledWaits(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer := observability.NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	tracer := poolAcquireTracer{observer: observer}
	for _, err := range []error{nil, context.Canceled} {
		ctx := context.WithValue(context.Background(), poolAcquireStartKey{}, time.Now().Add(-time.Second))
		tracer.TraceAcquireEnd(ctx, nil, pgxpool.TraceAcquireEndData{Err: err})
	}
	families, err := registry.Gather()
	if err != nil { t.Fatal(err) }
	seen := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "database_pool_acquire_seconds" { continue }
		for _, metric := range family.Metric {
			status := metric.Label[0].GetValue()
			seen[status] = true
			if metric.Histogram.GetSampleCount() != 1 || metric.Histogram.GetSampleSum() < 1 {
				t.Fatalf("%s pool wait was not recorded", status)
			}
		}
	}
	if !seen["success"] || !seen["failed"] { t.Fatal("success or cancellation metric missing") }
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
