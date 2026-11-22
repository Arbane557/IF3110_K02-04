//go:build integration

package migration

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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
			s.observer = observability.NewService(prometheus.NewRegistry(), provider.Tracer("worker-test"))
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
