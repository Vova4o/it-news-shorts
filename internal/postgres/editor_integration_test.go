//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
)

func TestEditorRepository_PersistenceAndIdempotency(t *testing.T) {
	cfg := testPGConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := postgres.MigrateUp(ctx, cfg.DSN); err != nil {
		t.Fatalf("MigrateUp() error = %v", err)
	}
	pool, err := postgres.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	defer pool.Close()

	suffix := uuid.NewString()[:8]
	snapID := "snap-edtest-" + suffix
	runID := "edrun-edtest-" + suffix
	artID1 := "art-ed1-" + suffix
	artID2 := "art-ed2-" + suffix
	storyID1 := "story-ed1-" + suffix
	storyID2 := "story-ed2-" + suffix
	testDate := time.Date(2099, 1, 15, 9, 0, 0, 0, time.UTC)

	cleanupDate := func(c context.Context) {
		_, _ = pool.Exec(c, `DELETE FROM daily_selections WHERE selection_date = $1::date`, testDate.Format("2006-01-02"))
		_, _ = pool.Exec(c, `DELETE FROM llm_usage WHERE editorial_run_id IN (SELECT id FROM editorial_runs WHERE run_date = $1::date)`, testDate.Format("2006-01-02"))
		_, _ = pool.Exec(c, `DELETE FROM stories WHERE editorial_run_id IN (SELECT id FROM editorial_runs WHERE run_date = $1::date)`, testDate.Format("2006-01-02"))
		_, _ = pool.Exec(c, `DELETE FROM editorial_runs WHERE run_date = $1::date`, testDate.Format("2006-01-02"))
		_, _ = pool.Exec(c, `DELETE FROM daily_snapshots WHERE snapshot_date = $1::date`, testDate.Format("2006-01-02"))
		_, _ = pool.Exec(c, `DELETE FROM articles WHERE id IN ($1, $2)`, artID1, artID2)
	}
	cleanupDate(ctx)

	t.Cleanup(func() {
		cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanCancel()
		cleanupDate(cleanCtx)
	})

	// Seed source, articles, and daily_snapshot for FK integrity.
	newsRepo := postgres.NewNewsRepository(pool, cfg.QueryTimeout)
	if err := newsRepo.EnsureSources(ctx, []news.SourceConfig{{
		ID: "techcrunch", Name: "TechCrunch", Type: news.SourceTypeRSS,
		FeedURL: "https://techcrunch.com/feed/", Language: domain.LanguageEnglish, Enabled: true,
	}}); err != nil {
		t.Fatalf("EnsureSources() error = %v", err)
	}

	pub := testDate.Add(-2 * time.Hour)
	for _, aid := range []string{artID1, artID2} {
		_, err := pool.Exec(ctx, `
			INSERT INTO articles (id, source_id, title, url, canonical_url, published_at, fetched_at, language)
			VALUES ($1, 'techcrunch', $2, $3, $3, $4, $4, 'en')
		`, aid, "Test Article "+aid, "https://example.com/"+aid, pub)
		if err != nil {
			t.Fatalf("insert test article %s: %v", aid, err)
		}
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO daily_snapshots (id, snapshot_date, window_start, window_end, coverage_status, article_count)
		VALUES ($1, $2::date, $3, $4, 'FULL_COVERAGE', 2)
		ON CONFLICT (snapshot_date) DO UPDATE SET
			window_start = EXCLUDED.window_start,
			window_end = EXCLUDED.window_end,
			article_count = EXCLUDED.article_count
		RETURNING id
	`, snapID, testDate.Format("2006-01-02"), testDate.Add(-24*time.Hour), testDate).Scan(&snapID)
	if err != nil {
		t.Fatalf("insert test snapshot: %v", err)
	}

	repo := postgres.NewEditorRepository(pool, cfg.QueryTimeout)

	stories := []editor.ClusteredStory{
		{
			ID:                 domain.StoryID(storyID1),
			Title:              "Verified AI Release",
			Summary:            "Verified AI Release summary",
			Category:           editor.CategoryAI,
			Fingerprint:        "fp-" + storyID1,
			Confidence:         0.95,
			ClusterReason:      "Single primary report",
			VerificationPassed: true,
			ArticleLinks:       []editor.StoryArticleLink{{ArticleID: artID1, Role: "PRIMARY", Confidence: 0.95, Reason: "Primary"}},
			Claims: []editor.Claim{
				{
					ID:                 storyID1 + "-claim-1",
					StoryID:            domain.StoryID(storyID1),
					Text:               "Model X released",
					Type:               editor.ClaimEvent,
					Entities:           []string{"Model X"},
					Confidence:         0.95,
					VerificationStatus: editor.StatusVerified,
					Evidence: []editor.ClaimEvidence{
						{
							ID:            "ev-" + storyID1 + "-1",
							ClaimID:       storyID1 + "-claim-1",
							ArticleID:     artID1,
							SourceURL:     "https://example.com/" + artID1,
							SourceName:    "techcrunch",
							Snippet:       "Model X released today",
							IsPrimary:     true,
							IsIndependent: true,
							AccessedAt:    pub,
						},
					},
				},
			},
			Score: &editor.StoryScore{
				StoryID:          domain.StoryID(storyID1),
				Impact:           90,
				Interest:         85,
				Freshness:        95,
				Novelty:          80,
				FinalScore:       88.75,
				ReasoningSummary: "High impact release",
				ModelVersion:     "mock-v1",
				PromptVersion:    "v1",
			},
		},
		{
			ID:                 domain.StoryID(storyID2),
			Title:              "Unverified Rumor",
			Summary:            "Unverified Rumor summary",
			Category:           editor.CategoryHardware,
			Fingerprint:        "fp-" + storyID2,
			Confidence:         0.80,
			ClusterReason:      "Unconfirmed rumor",
			VerificationPassed: false,
			RejectionReason:    "insufficient independent sources",
			ArticleLinks:       []editor.StoryArticleLink{{ArticleID: artID2, Role: "PRIMARY", Confidence: 0.80, Reason: "Primary"}},
		},
	}

	sel := editor.SelectionResult{
		Selected: []editor.SelectedStoryItem{
			{
				Rank:            1,
				StoryID:         storyID1,
				Title:           "Verified AI Release",
				Category:        "AI",
				Score:           88.75,
				SelectionReason: "High impact release",
				Fingerprint:     "fp-" + storyID1,
			},
		},
	}
	report := editor.EditorialReport{
		Date:              testDate.Format("2006-01-02"),
		EditorialRunID:    runID,
		SnapshotID:        snapID,
		WorkflowID:        "wf-test-" + suffix,
		Status:            "COMPLETED",
		ArticlesProcessed: 2,
		UniqueStories:     2,
		VerifiedStories:   1,
		RejectedStories:   1,
		Selected:          sel.Selected,
		LLMRequests:       2,
		InputTokens:       500,
		OutputTokens:      200,
		TotalCostUSD:      0.0012,
	}
	usage := []editor.LLMUsageRecord{
		{
			EditorialRunID: runID,
			Operation:      "clustering",
			Provider:       "mock",
			Model:          "mock-v1",
			PromptVersion:  "v1",
			RequestID:      "req-1-" + suffix,
			InputTokens:    300,
			OutputTokens:   100,
			CostUSD:        0.0007,
			DurationMS:     120,
			Status:         "OK",
		},
		{
			EditorialRunID: runID,
			Operation:      "scoring",
			Provider:       "mock",
			Model:          "mock-v1",
			PromptVersion:  "v1",
			RequestID:      "req-2-" + suffix,
			InputTokens:    200,
			OutputTokens:   100,
			CostUSD:        0.0005,
			DurationMS:     90,
			Status:         "OK",
		},
	}

	params := postgres.SaveEditorialResultsParams{
		RunID:         runID,
		SnapshotID:    snapID,
		RunDate:       testDate,
		Status:        "COMPLETED",
		Model:         "mock-v1",
		PromptVersion: "v1",
		Stories:       stories,
		Selection:     sel,
		Report:        report,
		Usage:         usage,
	}

	// Save twice to verify idempotency (Test F).
	if err := repo.SaveEditorialResults(ctx, params); err != nil {
		t.Fatalf("SaveEditorialResults(1) error = %v", err)
	}
	if err := repo.SaveEditorialResults(ctx, params); err != nil {
		t.Fatalf("SaveEditorialResults(2) idempotent rerun error = %v", err)
	}

	view, err := repo.GetDailySelection(ctx, testDate)
	if err != nil {
		t.Fatalf("GetDailySelection() error = %v", err)
	}
	if len(view.Items) != 1 || view.Items[0].StoryID != storyID1 {
		t.Fatalf("unexpected GetDailySelection items: %+v", view.Items)
	}

	exp, err := repo.ExplainStory(ctx, storyID1)
	if err != nil {
		t.Fatalf("ExplainStory() error = %v", err)
	}
	if exp.Score == nil || exp.Score.FinalScore != 88.75 || len(exp.Claims) != 1 {
		t.Fatalf("unexpected ExplainStory output: %+v", exp)
	}

	stats, err := repo.GetEditorialStats(ctx, testDate)
	if err != nil {
		t.Fatalf("GetEditorialStats() error = %v", err)
	}
	if stats.Runs != 1 || stats.SelectedCount != 1 || stats.LLMRequests != 2 {
		t.Fatalf("unexpected GetEditorialStats: %+v", stats)
	}

	prevFP, err := repo.GetPreviouslyPublishedFingerprints(ctx, testDate.Add(24*time.Hour), 7)
	if err != nil {
		t.Fatalf("GetPreviouslyPublishedFingerprints() error = %v", err)
	}
	if !prevFP["fp-"+storyID1] {
		t.Fatalf("expected fingerprint fp-%s in lookback map", storyID1)
	}
}
