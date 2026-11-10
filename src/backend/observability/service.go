package observability

import (
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

type Service struct {
	metrics *prometheusMetrics
	tracer  trace.Tracer
	logger  *slog.Logger
}

func NewService(registerer prometheus.Registerer, tracer trace.Tracer) *Service {
	return &Service{
		metrics: newPrometheusMetrics(registerer),
		tracer:  tracer,
		logger:  newLogger(),
	}
}
