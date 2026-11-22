package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestJSONLogsPreserveFieldsAndTraceCorrelation(t *testing.T) {
	for _, traced := range []bool{false, true} {
		t.Run(map[bool]string{false: "without trace", true: "with trace"}[traced], func(t *testing.T) {
			var output bytes.Buffer
			s := &Service{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			ctx := context.Background()
			parent := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled,
			})
			if traced {
				ctx = trace.ContextWithSpanContext(ctx, parent)
			}
			s.Log(ctx, slog.LevelError, "migration.query.finished", Fields{
				"actor": 7, "database": 4, "status": "failed", "sqlstate": "23505",
				"parameters": []any{}, "duration_ms": 12,
			})
			var entry map[string]any
			if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry["event"] != "migration.query.finished" || entry["level"] != "ERROR" ||
				entry["actor"] != float64(7) || entry["database"] != float64(4) ||
				entry["status"] != "failed" || entry["sqlstate"] != "23505" || entry["duration_ms"] != float64(12) {
				t.Fatalf("incorrect structured log: %+v", entry)
			}
			timestamp, ok := entry["time"].(string)
			if !ok {
				t.Fatal("log timestamp is missing or not a string")
			}
			if _, err := time.Parse(time.RFC3339Nano, timestamp); err != nil {
				t.Fatalf("invalid log timestamp: %v", err)
			}
			parameters, ok := entry["parameters"].([]any)
			if !ok || len(parameters) != 0 {
				t.Fatalf("parameters=%v", entry["parameters"])
			}
			if traced {
				if entry["trace_id"] != parent.TraceID().String() || entry["span_id"] != parent.SpanID().String() || entry["correlation_id"] != entry["trace_id"] {
					t.Fatalf("incorrect trace correlation: %+v", entry)
				}
			} else {
				for _, key := range []string{"trace_id", "span_id", "correlation_id"} {
					if _, exists := entry[key]; exists {
						t.Fatalf("untraced log includes %s", key)
					}
				}
			}
		})
	}
}

func TestSpansPreserveParentEventsAndErrorStatus(t *testing.T) {
	for _, failure := range []error{nil, errors.New("execution failed")} {
		name := "success"
		if failure != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
			s := NewService(prometheus.NewRegistry(), provider.Tracer("test"))
			ctx, parent := s.StartTrace(context.Background(), "api", nil)
			_, child := s.StartTrace(ctx, "query", Fields{"database": int64(4), "duration_ms": 1500 * time.Millisecond})
			child.AddEvent("batch.result", Fields{"rows": 5, "committed": true})
			child.End(failure)
			parent.End(nil)
			spans := recorder.Ended()
			if len(spans) != 2 {
				t.Fatalf("ended spans=%d", len(spans))
			}
			query, api := spans[0], spans[1]
			if query.Name() != "query" || query.Parent().SpanID() != api.SpanContext().SpanID() || query.SpanContext().TraceID() != api.SpanContext().TraceID() {
				t.Fatal("query did not retain API trace parent")
			}
			attributes := map[attribute.Key]attribute.Value{}
			for _, item := range query.Attributes() {
				attributes[item.Key] = item.Value
			}
			if attributes["database"].AsInt64() != 4 || attributes["duration_ms"].AsInt64() != 1500 {
				t.Fatalf("span attributes=%v", attributes)
			}
			events := query.Events()
			if len(events) == 0 || events[0].Name != "batch.result" {
				t.Fatalf("span events=%v", events)
			}
			eventFields := map[attribute.Key]attribute.Value{}
			for _, item := range events[0].Attributes {
				eventFields[item.Key] = item.Value
			}
			if eventFields["rows"].AsInt64() != 5 || !eventFields["committed"].AsBool() {
				t.Fatalf("batch event fields=%v", eventFields)
			}
			if failure == nil {
				if query.Status().Code != codes.Ok || len(events) != 1 {
					t.Fatalf("successful span status=%v events=%v", query.Status(), events)
				}
			} else if query.Status().Code != codes.Error || query.Status().Description != failure.Error() || len(events) != 2 || events[1].Name != "exception" {
				t.Fatalf("failed span status=%v events=%v", query.Status(), events)
			}
		})
	}
}

func TestMetricsAccumulateDurationsRowsAndReplacePoolGauges(t *testing.T) {
	registry := prometheus.NewRegistry()
	s := NewService(registry, trace.NewNoopTracerProvider().Tracer("test"))
	s.RecordMigration(MigrationResult{Direction: "up", Status: "success", Duration: 2 * time.Second})
	s.RecordMigration(MigrationResult{Direction: "up", Status: "success", Duration: time.Second})
	s.RecordMigration(MigrationResult{Direction: "rollback", Status: "failed", Duration: 500 * time.Millisecond})
	s.RecordBatch(BatchResult{Status: "success", Duration: time.Second, RowsAffected: 3})
	s.RecordBatch(BatchResult{Status: "success", Duration: 2 * time.Second, RowsAffected: 2})
	s.RecordBatch(BatchResult{Status: "failed", Duration: time.Second, RowsAffected: -1})
	s.SetDatabasePool(5, 2)
	s.SetDatabasePool(1, 4)

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			key := family.GetName()
			labels := make([]string, 0, len(metric.Label))
			for _, label := range metric.Label {
				switch label.GetName() {
				case "status", "direction", "state":
					labels = append(labels, label.GetName()+"="+label.GetValue())
				default:
					t.Fatalf("unexpected metric label %s", label.GetName())
				}
			}
			sort.Strings(labels)
			if len(labels) > 0 {
				key += "/" + strings.Join(labels, "/")
			}
			switch {
			case metric.Counter != nil:
				values[key] = metric.Counter.GetValue()
			case metric.Gauge != nil:
				values[key] = metric.Gauge.GetValue()
			case metric.Histogram != nil:
				values[key+"/count"] = float64(metric.Histogram.GetSampleCount())
				values[key+"/sum"] = metric.Histogram.GetSampleSum()
			}
		}
	}
	for key, want := range map[string]float64{
		"migration_executions_total/direction=up/status=success": 2,
		"migration_executions_total/direction=rollback/status=failed": 1,
		"migration_duration_seconds/direction=up/status=success/count": 2,
		"migration_duration_seconds/direction=up/status=success/sum": 3,
		"migration_duration_seconds/direction=rollback/status=failed/sum": 0.5,
		"migration_batches_total/status=success": 2,
		"migration_batches_total/status=failed": 1,
		"migration_batch_duration_seconds/status=success/sum": 3,
		"migration_batch_duration_seconds/status=success/count": 2,
		"migration_batch_duration_seconds/status=failed/count": 1,
		"migration_rows_processed_total/status=success": 5,
		"database_pool_connections/state=active": 1,
		"database_pool_connections/state=idle": 4,
	} {
		if got, exists := values[key]; !exists || got != want {
			t.Errorf("%s=%v present=%v want=%v", key, got, exists, want)
		}
	}
	if value := values["migration_rows_processed_total/status=failed"]; value != 0 {
		t.Fatalf("negative affected rows changed counter to %v", value)
	}
}
