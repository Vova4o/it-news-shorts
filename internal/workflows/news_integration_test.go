//go:build integration

package workflows_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func TestNewsIngestionWorkflow_PartialSourceFailure(t *testing.T) {
	cfg := liveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.Postgres.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg.Postgres)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	suffix := uuid.NewString()[:8]
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><item>
  <title>Partial Test Article %s</title>
  <link>https://example.com/partial/%s</link>
  <pubDate>%s</pubDate>
</item></channel></rss>`, suffix, suffix, time.Now().UTC().Add(-1*time.Hour).Format(time.RFC1123Z))
	}))
	defer okSrv.Close()

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failSrv.Close()

	okID := "wf-ok-" + suffix
	failID := "wf-fail-" + suffix
	repo := postgres.NewNewsRepository(pool, cfg.Postgres.QueryTimeout)
	sources := []news.SourceConfig{
		{ID: okID, Name: "OK RSS", Type: news.SourceTypeRSS, FeedURL: okSrv.URL, Language: domain.LanguageEnglish, Enabled: true, Limit: 10},
		{ID: failID, Name: "Fail RSS", Type: news.SourceTypeRSS, FeedURL: failSrv.URL, Language: domain.LanguageEnglish, Enabled: true, Limit: 10},
	}
	if err := repo.EnsureSources(ctx, sources); err != nil {
		t.Fatalf("EnsureSources() error = %v", err)
	}
	t.Cleanup(func() {
		cCtx, cCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cCancel()
		_, _ = pool.Exec(cCtx, `DELETE FROM articles WHERE source_id IN ($1, $2)`, okID, failID)
		_, _ = pool.Exec(cCtx, `DELETE FROM sources WHERE id IN ($1, $2)`, okID, failID)
	})

	httpClient := news.NewHTTPClient(news.HTTPClientOptions{
		Timeout:         2 * time.Second,
		MaxBytes:        1 << 20,
		MaxConcurrency:  4,
		MaxRetries:      0,
		AllowPrivateIPs: true,
	})
	acts := &workflows.NewsActivities{
		Repo:       repo,
		HTTPClient: httpClient,
		NewsConfig: config.NewsConfig{MaxConcurrency: 2},
		Sources:    sources,
		Metrics:    observability.NewMetrics(),
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)
	env.RegisterActivityWithOptions(acts.ListEnabledSources, activity.RegisterOptions{Name: "ListEnabledSources"})
	env.RegisterActivityWithOptions(acts.FetchAndPersistSource, activity.RegisterOptions{Name: "FetchAndPersistSource"})

	env.ExecuteWorkflow(workflows.NewsIngestionWorkflow, workflows.NewsIngestionInput{
		RunID:          "partial-" + suffix,
		SourceIDs:      []string{okID, failID},
		MaxConcurrency: 2,
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("expected partial success without workflow error, got %v", err)
	}

	var res workflows.NewsIngestionResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("GetWorkflowResult() error = %v", err)
	}
	if res.Status != "PARTIAL_FAILURE" {
		t.Fatalf("Status = %q, want PARTIAL_FAILURE", res.Status)
	}

	arts, err := repo.GetArticlesBySource(ctx, okID, 10)
	if err != nil || len(arts) != 1 {
		t.Fatalf("expected 1 persisted article for %s, got len=%d err=%v", okID, len(arts), err)
	}

	failState, err := repo.GetSourceFetchState(ctx, failID)
	if err != nil {
		t.Fatalf("GetSourceFetchState() error = %v", err)
	}
	if failState.LastError == "" || failState.ConsecutiveFailures < 1 {
		t.Fatalf("expected failure recorded for %s, got %+v", failID, failState)
	}
}
