package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type prometheusMetrics struct {
	migrationExecutions *prometheus.CounterVec
	migrationDuration   *prometheus.HistogramVec
	migrationBatches    *prometheus.CounterVec
	batchDuration       *prometheus.HistogramVec
	rowsProcessed       *prometheus.CounterVec
	poolConnections     *prometheus.GaugeVec
	queueWait           prometheus.Histogram
	lockWait            prometheus.Histogram
	poolWait            *prometheus.HistogramVec
	retries             prometheus.Counter
}

func newPrometheusMetrics(registerer prometheus.Registerer) *prometheusMetrics {
	metrics := &prometheusMetrics{
		queueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "migration_queue_wait_seconds", Help: "Time from queue eligibility to starting an execution attempt, including lock contention.",
			Buckets: []float64{0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60, 300},
		}),
		lockWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "migration_lock_wait_seconds", Help: "Elapsed target advisory lock acquisition time, including deferred deliveries.",
			Buckets: []float64{0.001, 0.01, 0.1, 0.5, 1, 2, 5, 10, 30, 60},
		}),
		poolWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "database_pool_acquire_seconds", Help: "Target connection acquisition latency, including waiting and connection establishment.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30},
		}, []string{"status"}),
		retries: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "migration_retries_total", Help: "Retry or recovery attempts actually started by this process.",
		}),
		migrationExecutions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "migration_executions_total",
			Help: "Total migration execution attempts.",
		}, []string{"status", "direction"}),
		migrationDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "migration_duration_seconds",
			Help:    "Migration execution duration in seconds.",
			Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300},
		}, []string{"status", "direction"}),
		migrationBatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "migration_batches_total",
			Help: "Total migration batches processed.",
		}, []string{"status"}),
		batchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "migration_batch_duration_seconds",
			Help:    "Migration batch processing duration in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60},
		}, []string{"status"}),
		rowsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "migration_rows_processed_total",
			Help: "Total rows affected by migration batches.",
		}, []string{"status"}),
		poolConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "database_pool_connections",
			Help: "Current target database pool connections.",
		}, []string{"state"}),
	}

	registerer.MustRegister(
		metrics.migrationExecutions,
		metrics.migrationDuration,
		metrics.migrationBatches,
		metrics.batchDuration,
		metrics.rowsProcessed,
		metrics.poolConnections,
		metrics.queueWait,
		metrics.lockWait,
		metrics.poolWait,
		metrics.retries,
	)
	return metrics
}

func (m *prometheusMetrics) recordMigration(result MigrationResult) {
	m.migrationExecutions.WithLabelValues(result.Status, result.Direction).Inc()
	m.migrationDuration.WithLabelValues(result.Status, result.Direction).Observe(result.Duration.Seconds())
}

func (m *prometheusMetrics) recordBatch(result BatchResult) {
	m.migrationBatches.WithLabelValues(result.Status).Inc()
	m.batchDuration.WithLabelValues(result.Status).Observe(result.Duration.Seconds())
	if result.RowsAffected > 0 {
		m.rowsProcessed.WithLabelValues(result.Status).Add(float64(result.RowsAffected))
	}
}

func (m *prometheusMetrics) setDatabasePool(active, idle int32) {
	m.poolConnections.WithLabelValues("active").Set(float64(active))
	m.poolConnections.WithLabelValues("idle").Set(float64(idle))
}

func (s *Service) RecordMigration(result MigrationResult) {
	s.metrics.recordMigration(result)
}

func (s *Service) RecordBatch(result BatchResult) {
	s.metrics.recordBatch(result)
}

func (s *Service) SetDatabasePool(active, idle int32) {
	s.metrics.setDatabasePool(active, idle)
}

func (s *Service) RecordQueueWait(duration time.Duration) {
	s.metrics.queueWait.Observe(max(duration.Seconds(), 0))
}

func (s *Service) RecordLockWait(duration time.Duration) {
	s.metrics.lockWait.Observe(max(duration.Seconds(), 0))
}

func (s *Service) RecordPoolWait(duration time.Duration, failed bool) {
	status := "success"
	if failed {
		status = "failed"
	}
	s.metrics.poolWait.WithLabelValues(status).Observe(max(duration.Seconds(), 0))
}

func (s *Service) RecordRetry() { s.metrics.retries.Inc() }

// Refresh aggregate target gauges at scrape time, including when no migration runs.
func (s *Service) MetricsHandler(gatherer prometheus.Gatherer, poolStats func() (int32, int32)) http.Handler {
	next := promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active, idle := poolStats()
		s.SetDatabasePool(active, idle)
		next.ServeHTTP(w, r)
	})
}
