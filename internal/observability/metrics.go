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
	Registry                  *prometheus.Registry
	ActivityDuration          *prometheus.HistogramVec
	WorkflowTotal             *prometheus.CounterVec
	ActivityRetryTotal        *prometheus.CounterVec
	PostgresErrors            *prometheus.CounterVec
	MinIOErrors               *prometheus.CounterVec
	DependencyDuration        *prometheus.HistogramVec
	NewsFetchTotal            *prometheus.CounterVec
	NewsFetchErrorsTotal      *prometheus.CounterVec
	NewsArticlesFetchedTotal  *prometheus.CounterVec
	NewsArticlesInsertedTotal *prometheus.CounterVec
	NewsDuplicatesTotal       *prometheus.CounterVec
	NewsFetchDurationSeconds  *prometheus.HistogramVec
	NewsSourceLastSuccessTS   *prometheus.GaugeVec
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
		NewsFetchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "news_fetch_total",
			Help: "Total number of news source fetch attempts.",
		}, []string{"source_id", "status"}),
		NewsFetchErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "news_fetch_errors_total",
			Help: "Total number of news source fetch errors.",
		}, []string{"source_id", "error_type"}),
		NewsArticlesFetchedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "news_articles_fetched_total",
			Help: "Total number of articles fetched from news sources.",
		}, []string{"source_id"}),
		NewsArticlesInsertedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "news_articles_inserted_total",
			Help: "Total number of new articles inserted into storage.",
		}, []string{"source_id"}),
		NewsDuplicatesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "news_duplicates_total",
			Help: "Total number of duplicate articles detected during ingestion.",
		}, []string{"source_id"}),
		NewsFetchDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "news_fetch_duration_seconds",
			Help:    "Duration of news source fetch operations in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"source_id", "status"}),
		NewsSourceLastSuccessTS: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "news_source_last_success_timestamp",
			Help: "Unix timestamp of the last successful fetch per news source.",
		}, []string{"source_id"}),
	}

	reg.MustRegister(
		m.ActivityDuration,
		m.WorkflowTotal,
		m.ActivityRetryTotal,
		m.PostgresErrors,
		m.MinIOErrors,
		m.DependencyDuration,
		m.NewsFetchTotal,
		m.NewsFetchErrorsTotal,
		m.NewsArticlesFetchedTotal,
		m.NewsArticlesInsertedTotal,
		m.NewsDuplicatesTotal,
		m.NewsFetchDurationSeconds,
		m.NewsSourceLastSuccessTS,
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
