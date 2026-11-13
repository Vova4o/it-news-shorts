package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus collectors for workflows, activities, and
// infrastructure dependencies.
type Metrics struct {
	Registry           *prometheus.Registry
	ActivityDuration   *prometheus.HistogramVec
	WorkflowTotal      *prometheus.CounterVec
	ActivityRetryTotal *prometheus.CounterVec
	PostgresErrors     *prometheus.CounterVec
	MinIOErrors        *prometheus.CounterVec
	DependencyDuration *prometheus.HistogramVec
}

// NewMetrics creates a new isolated Prometheus registry and registers all
// service metrics.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{
		Registry: reg,
		ActivityDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "it_news",
			Name:      "activity_duration_seconds",
			Help:      "Duration of Temporal activity executions in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"activity", "status"}),
		WorkflowTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "it_news",
			Name:      "workflow_total",
			Help:      "Total number of completed or failed Temporal workflows.",
		}, []string{"workflow", "status"}),
		ActivityRetryTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "it_news",
			Name:      "activity_retry_total",
			Help:      "Total number of Temporal activity retry attempts (attempt > 1).",
		}, []string{"activity"}),
		PostgresErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "it_news",
			Name:      "postgres_errors_total",
			Help:      "Total number of PostgreSQL operation errors.",
		}, []string{"operation"}),
		MinIOErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "it_news",
			Name:      "minio_errors_total",
			Help:      "Total number of MinIO/S3 operation errors.",
		}, []string{"operation"}),
		DependencyDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "it_news",
			Name:      "dependency_duration_seconds",
			Help:      "Duration of calls to external infrastructure dependencies in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"dependency", "operation", "status"}),
	}

	reg.MustRegister(
		m.ActivityDuration,
		m.WorkflowTotal,
		m.ActivityRetryTotal,
		m.PostgresErrors,
		m.MinIOErrors,
		m.DependencyDuration,
	)

	return m
}

// Handler returns an HTTP handler exposing the Prometheus metrics endpoint.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// ObserveDependency records latency and optional error count for a dependency call.
func (m *Metrics) ObserveDependency(dependency, operation string, elapsed time.Duration, err error) {
	if m == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
		switch dependency {
		case "postgres":
			m.PostgresErrors.WithLabelValues(operation).Inc()
		case "minio":
			m.MinIOErrors.WithLabelValues(operation).Inc()
		}
	}
	m.DependencyDuration.WithLabelValues(dependency, operation, status).Observe(elapsed.Seconds())
}
