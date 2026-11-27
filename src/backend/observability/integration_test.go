//go:build integration && observability

package observability

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

// This suite needs Collector and Loki, not PostgreSQL. The observability-test
// Compose profile supplies their internal URLs without exposing Loki to the host.
func TestCollectorStoresStructuredLogsInLokiIntegration(t *testing.T) {
	collectorURL := strings.TrimRight(os.Getenv("INTEGRATION_OTEL_HTTP_URL"), "/")
	lokiURL := strings.TrimRight(os.Getenv("INTEGRATION_LOKI_URL"), "/")
	if collectorURL == "" || lokiURL == "" {
		t.Fatal("observability integration tests require INTEGRATION_OTEL_HTTP_URL and INTEGRATION_LOKI_URL")
	}
	for _, serviceName := range []string{"wbd-backend", "wbd-migration-worker"} {
		t.Run(serviceName, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			marker := hex.EncodeToString(nonce[:])
			spanContext := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID(nonce), SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled,
			})
			ctx = trace.ContextWithSpanContext(ctx, spanContext)
			var output bytes.Buffer
			s := &Service{logger: slog.New(slog.NewJSONHandler(&output, nil))}
			s.Log(ctx, slog.LevelInfo, "central_storage.integration", Fields{
				"test_marker": marker, "database": 1, "actor": 7, "status": "success",
			})
			line := strings.TrimSpace(output.String())
			started := time.Now().Add(-time.Minute)
			payload, err := proto.Marshal(&collectorlogsv1.ExportLogsServiceRequest{
				ResourceLogs: []*logsv1.ResourceLogs{{
					Resource: &resourcev1.Resource{Attributes: []*commonv1.KeyValue{{
						Key: "service.name", Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: serviceName}},
					}}},
					ScopeLogs: []*logsv1.ScopeLogs{{LogRecords: []*logsv1.LogRecord{{
						TimeUnixNano: uint64(time.Now().UnixNano()),
						SeverityNumber: logsv1.SeverityNumber_SEVERITY_NUMBER_INFO,
						SeverityText: "INFO",
						Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: line}},
					}}}},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Timeout: 3 * time.Second}
			var lastError error
			accepted := false
			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, collectorURL+"/v1/logs", bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/x-protobuf")
				response, err := client.Do(req)
				lastError = err
				if err == nil {
					body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
					response.Body.Close()
					if response.StatusCode == http.StatusOK && readErr == nil {
						var result collectorlogsv1.ExportLogsServiceResponse
						if err := proto.Unmarshal(body, &result); err != nil {
							t.Fatal(err)
						}
						if result.GetPartialSuccess().GetRejectedLogRecords() != 0 {
							t.Fatal("Collector rejected integration log records")
						}
						accepted = true
						break
					}
					lastError = fmt.Errorf("Collector status=%d read error=%v", response.StatusCode, readErr)
				}
				if !waitForTelemetry(ctx) {
					break
				}
			}
			if !accepted {
				t.Fatalf("Collector did not accept logs: %v", lastError)
			}
			for ctx.Err() == nil {
				params := url.Values{
					"query": {fmt.Sprintf(`{service_name=%q} |= %q`, serviceName, marker)},
					"start": {strconv.FormatInt(started.UnixNano(), 10)},
					"end": {strconv.FormatInt(time.Now().UnixNano(), 10)},
					"limit": {"100"},
				}
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, lokiURL+"/loki/api/v1/query_range?"+params.Encode(), nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(req)
				lastError = err
				if err == nil {
					var result struct {
						Status string `json:"status"`
						Data struct {
							Result []struct {
								Stream map[string]string `json:"stream"`
								Values [][]json.RawMessage `json:"values"`
							} `json:"result"`
						} `json:"data"`
					}
					decodeErr := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result)
					response.Body.Close()
					lastError = fmt.Errorf("Loki status=%d decode error=%v", response.StatusCode, decodeErr)
					if response.StatusCode == http.StatusOK && decodeErr == nil && result.Status == "success" {
						for _, stream := range result.Data.Result {
							for _, value := range stream.Values {
								if len(value) < 2 {
									continue
								}
								var storedLine string
								if err := json.Unmarshal(value[1], &storedLine); err != nil {
									t.Fatalf("invalid stored log line: %v", err)
								}
								if storedLine == line {
									if stream.Stream["service_name"] != serviceName {
										t.Fatal("stored log attributed to the wrong service")
									}
									for _, key := range []string{"test_marker", "trace_id", "actor", "database", "job_id", "execution_id"} {
										if _, indexed := stream.Stream[key]; indexed {
											t.Fatalf("high cardinality field %s became an index label", key)
										}
									}
									return
								}
							}
						}
						lastError = fmt.Errorf("Loki has not returned the stored JSON log yet")
					}
				}
				if !waitForTelemetry(ctx) {
					break
				}
			}
			t.Fatalf("central log was not readable from Loki: %v", lastError)
		})
	}
}

func waitForTelemetry(ctx context.Context) bool {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
