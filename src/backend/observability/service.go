package observability

import "github.com/prometheus/client_golang/prometheus"

type Service struct {
	metrics *prometheusMetrics
}

func NewService(registerer prometheus.Registerer) *Service {
	return &Service{metrics: newPrometheusMetrics(registerer)}
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
