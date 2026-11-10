package observability

import (
	"github.com/prometheus/client_golang/prometheus"
)

type prometheusMetrics struct {
	migrationExecutions *prometheus.CounterVec
	migrationDuration   *prometheus.HistogramVec
	migrationBatches    *prometheus.CounterVec
	batchDuration       *prometheus.HistogramVec
	rowsProcessed       *prometheus.CounterVec
	poolConnections     *prometheus.GaugeVec
}

func newPrometheusMetrics(registerer prometheus.Registerer) *prometheusMetrics {
	metrics := &prometheusMetrics{
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
