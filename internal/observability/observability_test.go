package observability_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/observability"
)

func TestLoggerOutputsStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	logger := observability.NewLogger("worker", "info", &buf)
	logger.Info("activity completed",
		"workflow_id", "wf-1",
		"run_id", "run-1",
		"video_job_id", "job-1",
		"activity", "CreateSmokeRun",
		"duration_ms", 12,
	)

	var payload map[string]any
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal log json: %v", err)
	}
	for _, key := range []string{"timestamp", "level", "service", "workflow_id", "run_id", "video_job_id", "activity", "duration_ms"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("missing log key %q in %v", key, payload)
		}
	}
	if payload["service"] != "worker" {
		t.Fatalf("service = %v, want worker", payload["service"])
	}
}

func TestMetricsExposesPrometheusText(t *testing.T) {
	m := observability.NewMetrics()
	m.WorkflowTotal.WithLabelValues("InfrastructureSmokeWorkflow", "completed").Inc()
	m.ActivityDuration.WithLabelValues("CreateSmokeRun", "ok").Observe(0.05)
	m.ActivityRetryTotal.WithLabelValues("UploadAndVerifyArtifact").Inc()
	m.ObserveDependency("postgres", "create_daily_run", 10*time.Millisecond, errors.New("timeout"))
	m.ObserveDependency("minio", "put_object", 20*time.Millisecond, errors.New("unavailable"))

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, metric := range []string{
		"it_news_workflow_total",
		"it_news_activity_duration_seconds",
		"it_news_activity_retry_total",
		"it_news_postgres_errors_total",
		"it_news_minio_errors_total",
		"it_news_dependency_duration_seconds",
	} {
		if !strings.Contains(body, metric) {
			t.Fatalf("metrics output missing %q", metric)
		}
	}
}
