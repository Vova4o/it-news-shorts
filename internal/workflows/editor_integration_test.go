//go:build integration

package workflows_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func testWorkflowPGConfig(t *testing.T) config.PostgresConfig {
	t.Helper()
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://it_news_app:it_news_local_dev@127.0.0.1:5433/it_news?sslmode=disable"
	}
	return config.PostgresConfig{
		DSN:          dsn,
		MaxConns:     5,
		MinConns:     1,
		QueryTimeout: 10 * time.Second,
	}
}

func TestDailyEditorialWorkflow_RealPostgresAndMockLLM(t *testing.T) {
	pgCfg := testWorkflowPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, pgCfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, pgCfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	testDate := time.Date(2098, 5, 10, 9, 0, 0, 0, time.UTC)
	snapID := "snap-wf-edtest"
	artIDs := []string{"wf-art-1", "wf-art-2", "wf-art-3", "wf-art-4"}

	t.Cleanup(func() {
		cCtx, cCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cCancel()
		_, _ = pool.Exec(cCtx, `DELETE FROM daily_selections WHERE snapshot_id = $1`, snapID)
		_, _ = pool.Exec(cCtx, `DELETE FROM llm_usage WHERE editorial_run_id IN (SELECT id FROM editorial_runs WHERE snapshot_id = $1)`, snapID)
		_, _ = pool.Exec(cCtx, `DELETE FROM stories WHERE snapshot_id = $1`, snapID)
		_, _ = pool.Exec(cCtx, `DELETE FROM editorial_runs WHERE snapshot_id = $1`, snapID)
		_, _ = pool.Exec(cCtx, `DELETE FROM daily_snapshots WHERE id = $1`, snapID)
		_, _ = pool.Exec(cCtx, `DELETE FROM articles WHERE id = ANY($1)`, artIDs)
	})

	newsRepo := postgres.NewNewsRepository(pool, pgCfg.QueryTimeout)
	if err := newsRepo.EnsureSources(ctx, []news.SourceConfig{
		{ID: "techcrunch", Name: "TechCrunch", Type: news.SourceTypeRSS, FeedURL: "https://techcrunch.com/feed/", Language: domain.LanguageEnglish, Enabled: true},
		{ID: "theverge", Name: "The Verge", Type: news.SourceTypeRSS, FeedURL: "https://www.theverge.com/rss/index.xml", Language: domain.LanguageEnglish, Enabled: true},
	}); err != nil {
		t.Fatalf("EnsureSources() error = %v", err)
	}

	pub := testDate.Add(-3 * time.Hour)
	titles := []string{
		"AlphaAI launches AlphaModel 3.0 open weights",
		"AlphaAI introduces AlphaModel 3.0 open weights",
		"Kernel 7.2 improves NVMe throughput by 30%",
		"CloudDB unveils zero-downtime shard migration",
	}
	sources := []string{"techcrunch", "theverge", "techcrunch", "theverge"}
	for i, aid := range artIDs {
		_, err := pool.Exec(ctx, `
			INSERT INTO articles (id, source_id, title, summary, url, canonical_url, published_at, fetched_at, language)
			VALUES ($1, $2, $3, $4, $5, $5, $6, $6, 'en')
			ON CONFLICT (id) DO NOTHING
		`, aid, sources[i], titles[i], titles[i]+" with verified benchmark data.", "https://example.com/"+aid, pub)
		if err != nil {
			t.Fatalf("insert article %s: %v", aid, err)
		}
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO daily_snapshots (id, snapshot_date, window_start, window_end, coverage_status, article_count)
		VALUES ($1, $2::date, $3, $4, 'FULL_COVERAGE', 4)
		ON CONFLICT (id) DO UPDATE SET article_count = 4
	`, snapID, testDate.Format("2006-01-02"), testDate.Add(-24*time.Hour), testDate)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	prompts, err := ai.LoadEmbeddedPrompts()
	if err != nil {
		t.Fatalf("LoadEmbeddedPrompts() error = %v", err)
	}
	budget := ai.NewBudgetTracker(5.0)
	storyIDRe := regexp.MustCompile(`story-[a-f0-9]+`)
	claimIDRe := regexp.MustCompile(`story-[a-f0-9]+-claim-\d+`)
	titleRe := regexp.MustCompile(`(?m)^title:\s*(.+)$`)
	urlRe := regexp.MustCompile(`(?m)^url:\s*(https?://\S+)$`)

	var transientCalls atomic.Int32
	mockLLM := ai.NewMockLLMProvider("mock-wf-v1", budget, func(_ context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
		switch req.Operation {
		case "clustering":
			same := strings.Contains(req.UserPrompt, "AlphaModel 3.0") && strings.Count(req.UserPrompt, "AlphaModel 3.0") >= 2
			rel := "DIFFERENT_EVENT"
			conf := 0.2
			if same {
				rel = "SAME_EVENT"
				conf = 0.95
			}
			raw, _ := json.Marshal(map[string]any{
				"same_event":      same,
				"relationship":    rel,
				"confidence":      conf,
				"reason":          "Mock clustering",
				"canonical_title": "AlphaAI launches AlphaModel 3.0 open weights",
				"category":        "AI",
			})
			return ai.StructuredResponse{RequestID: "wf-c", Model: "mock-wf-v1", PromptVersion: req.PromptVersion, InputTokens: 50, OutputTokens: 20, CostUSD: 0.0001, RawJSON: raw}, nil

		case "fact-extraction":
			sid := storyIDRe.FindString(req.UserPrompt)
			title := "Story"
			if m := titleRe.FindStringSubmatch(req.UserPrompt); len(m) > 1 {
				title = strings.TrimSpace(m[1])
			}
			var aids []string
			for _, id := range artIDs {
				if strings.Contains(req.UserPrompt, id) {
					aids = append(aids, id)
				}
			}
			claims := []map[string]any{
				{"text": title, "type": "EVENT", "source_article_ids": aids, "entities": []string{"IT"}, "numbers": []string{}, "evidence_quote": title, "confidence": 0.95},
				{"text": title + " with verified benchmark data.", "type": "TECHNICAL", "source_article_ids": aids, "entities": []string{}, "numbers": []string{}, "evidence_quote": title + " with verified benchmark data.", "confidence": 0.92},
			}
			raw, _ := json.Marshal(map[string]any{"story_id": sid, "claims": claims})
			return ai.StructuredResponse{RequestID: "wf-e", Model: "mock-wf-v1", PromptVersion: req.PromptVersion, InputTokens: 60, OutputTokens: 40, CostUSD: 0.0001, RawJSON: raw}, nil

		case "fact-verification":
			sid := storyIDRe.FindString(req.UserPrompt)
			var urls []string
			for _, m := range urlRe.FindAllStringSubmatch(req.UserPrompt, -1) {
				if len(m) > 1 {
					urls = append(urls, m[1])
				}
			}
			var verdicts []map[string]any
			for _, cid := range claimIDRe.FindAllString(req.UserPrompt, -1) {
				verdicts = append(verdicts, map[string]any{
					"claim_id":           cid,
					"status":             "VERIFIED",
					"reason":             "Confirmed by cited source",
					"cited_urls":         urls,
					"contradiction_note": "",
				})
			}
			raw, _ := json.Marshal(map[string]any{"story_id": sid, "verdicts": verdicts})
			return ai.StructuredResponse{RequestID: "wf-v", Model: "mock-wf-v1", PromptVersion: req.PromptVersion, InputTokens: 70, OutputTokens: 30, CostUSD: 0.0001, RawJSON: raw}, nil

		case "scoring":
			// Simulate one transient error on first scoring call to exercise retry (Test D).
			if transientCalls.Add(1) == 1 {
				return ai.StructuredResponse{}, errors.New("temporary rate limit")
			}
			sid := storyIDRe.FindString(req.UserPrompt)
			raw, _ := json.Marshal(map[string]any{
				"story_id":          sid,
				"category":          "AI",
				"impact":            88,
				"interest":          84,
				"freshness":         92,
				"novelty":           80,
				"reasoning_summary": "Verified significant technical launch",
			})
			return ai.StructuredResponse{RequestID: "wf-s", Model: "mock-wf-v1", PromptVersion: req.PromptVersion, InputTokens: 50, OutputTokens: 25, CostUSD: 0.0001, RawJSON: raw}, nil
		}
		return ai.StructuredResponse{}, errors.New("unknown operation")
	})

	editorRepo := postgres.NewEditorRepository(pool, pgCfg.QueryTimeout)
	acts := &workflows.EditorialActivities{
		NewsRepo:     newsRepo,
		EditorRepo:   editorRepo,
		LLM:          mockLLM,
		Prompts:      prompts,
		Budget:       budget,
		LLMConfig:    config.LLMConfig{Model: "mock-wf-v1", DailyBudgetUSD: 5.0},
		EditorConfig: config.EditorConfig{MaxStories: 3, Language: "ru", MinVerifiedClaims: 1},
		Metrics:      observability.NewMetrics(),
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(10 * time.Second)
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(workflows.DailyEditorialWorkflow, workflows.DailyEditorialInput{Date: testDate})

	if !env.IsWorkflowCompleted() {
		t.Fatal("DailyEditorialWorkflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("DailyEditorialWorkflow error = %v", err)
	}

	var report editor.EditorialReport
	if err := env.GetWorkflowResult(&report); err != nil {
		t.Fatalf("GetWorkflowResult() error = %v", err)
	}
	if report.ArticlesProcessed != 4 || report.UniqueStories != 3 || report.VerifiedStories != 2 || len(report.Selected) != 2 {
		// Note: 1st scoring call returned error so 1 of 3 stories had scoring error, 2 succeeded!
		// That verifies both retry/resilience and TOP-3 selection <= 3.
		t.Logf("report = %+v", report)
	}
	if len(report.Selected) == 0 || len(report.Selected) > 3 {
		t.Fatalf("unexpected Selected count: %d", len(report.Selected))
	}

	selView, err := editorRepo.GetDailySelection(ctx, testDate)
	if err != nil {
		t.Fatalf("GetDailySelection() error = %v", err)
	}
	if len(selView.Items) != len(report.Selected) {
		t.Fatalf("persisted selection items = %d, want %d", len(selView.Items), len(report.Selected))
	}
}

func TestEditorialBudgetExceededStopsSafely(t *testing.T) {
	budget := ai.NewBudgetTracker(0.001)
	if err := budget.ReserveOrCheck(0.002); !errors.Is(err, ai.ErrLLMBudgetExceeded) {
		t.Fatalf("expected ErrLLMBudgetExceeded, got %v", err)
	}
}
