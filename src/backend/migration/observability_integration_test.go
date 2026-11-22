//go:build integration

package migration

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"wbd/backend/observability"
)

func TestAsyncWorkerObservabilityIntegration(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "commit"
		if fail {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			s, target := asyncFixture(t)
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			registry := prometheus.NewRegistry()
			s.observer = observability.NewService(registry, provider.Tracer("worker-test"))
			registry.MustRegister(NewJobCollector(s.internalDB))
			ctx := context.Background()
			if _, err := target.Pool.Exec(ctx, "CREATE TABLE items(id bigint PRIMARY KEY, value int); INSERT INTO items VALUES(1,0)"); err != nil {
				t.Fatal(err)
			}
			query := "UPDATE items SET value=1"
			if fail {
				query += "; SELECT * FROM missing_relation"
			}
			m, err := s.Create(ctx, 1, 1, query)
			if err != nil {
				t.Fatal(err)
			}
			requestCtx, requestSpan := provider.Tracer("api-test").Start(ctx, "api.request")
			requestContext := requestSpan.SpanContext()
			job, err := s.SubmitJob(requestCtx, 1, m.Version, 1, "up", BatchOptions{})
			requestSpan.End()
			if err != nil {
				t.Fatal(err)
			}
			stored, err := s.GetJob(ctx, 1, job.ID)
			if err != nil || stored.traceparent == "" {
				t.Fatalf("queue lost trace context: error=%v", err)
			}
			if got := observedMetric(t, registry, "migration_jobs", map[string]string{"status":"pending"}).GetGauge().GetValue(); got != 1 {
				t.Fatalf("pending jobs=%v", got)
			}
			// A new worker context must recover the API trace from durable job data.
			if err := s.processJob(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			status, expectedValue := "success", 1
			batchCode := codes.Ok
			if fail {
				status, expectedValue, batchCode = "failed", 0, codes.Error
			}
			requireJobStatus(t, s, job.ID, status)
			// Redelivery of a completed message must not increment execution or row counters.
			if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
			if got := observedMetric(t, registry, "migration_jobs", map[string]string{"status":status}).GetGauge().GetValue(); got != 1 {
				t.Fatalf("persisted %s jobs=%v", status, got)
			}
			if got := observedMetric(t, registry, "migration_jobs", map[string]string{"status":"pending"}).GetGauge().GetValue(); got != 0 {
				t.Fatalf("pending gauge did not clear: %v", got)
			}
			if got := observedMetric(t, registry, "migration_executions_total", map[string]string{"status":status,"direction":"up"}).GetCounter().GetValue(); got != 1 {
				t.Fatalf("attempt counter=%v", got)
			}
			if got := observedMetric(t, registry, "migration_batches_total", map[string]string{"status":status}).GetCounter().GetValue(); got != 1 {
				t.Fatalf("batch counter=%v", got)
			}
			for _, metric := range []string{"migration_queue_wait_seconds", "migration_lock_wait_seconds", "migration_duration_seconds"} {
				if count := observedMetric(t, registry, metric, nil).GetHistogram().GetSampleCount(); count != 1 {
					t.Fatalf("%s observations=%d", metric, count)
				}
			}
			if !fail {
				if rows := observedMetric(t, registry, "migration_rows_processed_total", map[string]string{"status":"success"}).GetCounter().GetValue(); rows != 1 {
					t.Fatalf("committed row count=%v", rows)
				}
			}
			var value int
			if err := target.Pool.QueryRow(ctx, "SELECT value FROM items WHERE id=1").Scan(&value); err != nil || value != expectedValue {
				t.Fatalf("target value=%d want=%d error=%v", value, expectedValue, err)
			}
			spans := map[string]sdktrace.ReadOnlySpan{}
			for _, span := range recorder.Ended() {
				spans[span.Name()] = span
			}
			for _, name := range []string{"migration.queue.submit", "migration.worker", "migration.target.lock", "migration.batch"} {
				span, exists := spans[name]
				if !exists {
					t.Fatalf("missing span %s", name)
				}
				if span.SpanContext().TraceID() != requestContext.TraceID() {
					t.Fatalf("%s lost API trace", name)
				}
			}
			worker, batch := spans["migration.worker"], spans["migration.batch"]
			if worker.Parent().SpanID() != requestContext.SpanID() || batch.Parent().SpanID() != worker.SpanContext().SpanID() {
				t.Fatal("worker/batch trace hierarchy is broken")
			}
			if batch.Status().Code != batchCode || batch.EndTime().Before(batch.StartTime()) {
				t.Fatalf("batch status=%v duration=%v", batch.Status(), batch.EndTime().Sub(batch.StartTime()))
			}
			attributes := map[string]int64{}
			for _, item := range batch.Attributes() {
				switch string(item.Key) {
				case "job_id", "execution_id", "actor", "database":
					attributes[string(item.Key)] = item.Value.AsInt64()
				}
			}
			history, err := s.ListHistory(ctx, 1, false)
			if err != nil || len(history) != 1 || attributes["job_id"] != job.ID ||
				attributes["execution_id"] != history[0].ExecutionID || attributes["actor"] != 1 || attributes["database"] != 1 {
				t.Fatalf("span cannot be correlated to history: attributes=%v history=%+v error=%v", attributes, history, err)
			}
		})
	}
}

func observedMetric(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := registry.Gather()
	if err != nil { t.Fatal(err) }
	for _, family := range families {
		if family.GetName() != name { continue }
		for _, metric := range family.Metric {
			actual := map[string]string{}
			for _, label := range metric.Label { actual[label.GetName()] = label.GetValue() }
			matches := true
			for key, value := range labels {
				if actual[key] != value { matches = false }
			}
			if matches { return metric }
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return nil
}

func TestQueueAndLockWaitSurviveDeferredDeliveryIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	registry := prometheus.NewRegistry()
	s.observer = observability.NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	m, err := s.Create(ctx, 1, 1, "SELECT 1")
	if err != nil { t.Fatal(err) }
	job, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{})
	if err != nil { t.Fatal(err) }
	conn, err := target.Pool.Acquire(ctx)
	if err != nil { t.Fatal(err) }
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock("+targetLock+")"); err != nil { t.Fatal(err) }
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock_all()")
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "pending")
	// Age persisted clocks instead of sleeping or depending on scheduler timing.
	if _, err := s.internalDB.Exec(`UPDATE migration_jobs SET queued_at=now()-interval '3 seconds',
        lock_wait_started_at=now()-interval '2 seconds' WHERE id=$1`, job.ID); err != nil { t.Fatal(err) }
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock("+targetLock+")"); err != nil { t.Fatal(err) }
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "success")
	if got := observedMetric(t, registry, "migration_lock_wait_seconds", nil).GetHistogram().GetSampleSum(); got < 2 {
		t.Fatalf("deferred lock wait=%v", got)
	}
	if got := observedMetric(t, registry, "migration_queue_wait_seconds", nil).GetHistogram().GetSampleSum(); got < 3 {
		t.Fatalf("queue wait=%v", got)
	}
}

func TestAsyncRetryMetricsIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	registry := prometheus.NewRegistry()
	s.observer = observability.NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	if _, err := target.Pool.Exec(ctx, "CREATE SEQUENCE attempt_number"); err != nil { t.Fatal(err) }
	m, err := s.Create(ctx, 1, 1, `DO $$ BEGIN
        IF nextval('attempt_number')=1 THEN
            RAISE EXCEPTION 'retry once' USING ERRCODE='40001';
        END IF;
        END $$`)
	if err != nil { t.Fatal(err) }
	job, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{})
	if err != nil { t.Fatal(err) }
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "pending")
	if _, err := s.internalDB.Exec("UPDATE migration_jobs SET available_at=now(),queued_at=now() WHERE id=$1", job.ID); err != nil { t.Fatal(err) }
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "success")
	if got := observedMetric(t, registry, "migration_retries_total", nil).GetCounter().GetValue(); got != 1 { t.Fatalf("retries=%v", got) }
	for _, status := range []string{"failed", "success"} {
		if got := observedMetric(t, registry, "migration_executions_total", map[string]string{"status":status,"direction":"up"}).GetCounter().GetValue(); got != 1 {
			t.Fatalf("%s attempts=%v", status, got)
		}
	}
}

func TestJobMetricsReportDatabaseFailureIntegration(t *testing.T) {
	s, _ := asyncFixture(t)
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewJobCollector(s.internalDB))
	if _, err := s.internalDB.Exec("ALTER TABLE migration_jobs RENAME TO unavailable_jobs"); err != nil { t.Fatal(err) }
	if got := observedMetric(t, registry, "migration_job_metrics_scrape_success", nil).GetGauge().GetValue(); got != 0 {
		t.Fatalf("failed scrape reported success=%v", got)
	}
}

func TestBatchMetricsCountOnlyCommittedRowsAndExcludeCompletionMarkersIntegration(t *testing.T) {
	ctx := context.Background()
	s, target := asyncFixture(t)
	registry := prometheus.NewRegistry()
	s.observer = observability.NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	if _, err := target.Pool.Exec(ctx, `CREATE TABLE items(id bigint PRIMARY KEY,value int DEFAULT 0,
        CONSTRAINT fail_batch CHECK(id<3 OR value=0)); INSERT INTO items(id) VALUES(1),(2),(3),(4)`); err != nil { t.Fatal(err) }
	m, err := s.Create(ctx, 1, 1, "UPDATE items SET value=value+1 WHERE id = ANY($1::bigint[])")
	if err != nil { t.Fatal(err) }
	job, err := s.SubmitJob(ctx, 1, m.Version, 1, "up", BatchOptions{"batch","items",2})
	if err != nil { t.Fatal(err) }
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "partial")
	if got := observedMetric(t, registry, "migration_rows_processed_total", map[string]string{"status":"success"}).GetCounter().GetValue(); got != 2 {
		t.Fatalf("partial committed rows=%v", got)
	}
	if got := observedMetric(t, registry, "migration_batches_total", map[string]string{"status":"failed"}).GetCounter().GetValue(); got != 1 {
		t.Fatalf("failed batches=%v", got)
	}
	if _, err := target.Pool.Exec(ctx, "ALTER TABLE items DROP CONSTRAINT fail_batch"); err != nil { t.Fatal(err) }
	if _, err := s.RecoverJob(ctx, 1, job.ID, 1, false); err != nil { t.Fatal(err) }
	if err := s.processJob(ctx, job.ID); err != nil { t.Fatal(err) }
	requireJobStatus(t, s, job.ID, "success")
	if got := observedMetric(t, registry, "migration_rows_processed_total", map[string]string{"status":"success"}).GetCounter().GetValue(); got != 4 {
		t.Fatalf("resume recounted committed rows: %v", got)
	}
	if got := observedMetric(t, registry, "migration_batches_total", map[string]string{"status":"success"}).GetCounter().GetValue(); got != 2 {
		t.Fatalf("completion marker counted as data batch: %v", got)
	}
}
