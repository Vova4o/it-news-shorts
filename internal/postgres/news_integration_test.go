//go:build integration

package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
)

func TestNewsRepository_PersistenceDedupParallelAndSnapshot(t *testing.T) {
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

	repo := postgres.NewNewsRepository(pool, cfg.QueryTimeout)
	suffix := uuid.NewString()[:8]
	src1 := news.SourceConfig{
		ID:       "test-rss-" + suffix,
		Name:     "Test RSS",
		Type:     news.SourceTypeRSS,
		FeedURL:  "https://example.com/rss-" + suffix,
		Language: domain.LanguageEnglish,
		Enabled:  true,
	}
	src2 := news.SourceConfig{
		ID:       "test-hn-" + suffix,
		Name:     "Test HN",
		Type:     news.SourceTypeHackerNews,
		FeedURL:  "https://example.com/hn-" + suffix,
		Language: domain.LanguageEnglish,
		Enabled:  true,
	}
	if err := repo.EnsureSources(ctx, []news.SourceConfig{src1, src2}); err != nil {
		t.Fatalf("EnsureSources() error = %v", err)
	}
	t.Cleanup(func() {
		cCtx, cCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cCancel()
		_, _ = pool.Exec(cCtx, `DELETE FROM daily_snapshots WHERE workflow_id = $1`, "test-wf-"+suffix)
		_, _ = pool.Exec(cCtx, `DELETE FROM articles WHERE source_id IN ($1, $2)`, src1.ID, src2.ID)
		_, _ = pool.Exec(cCtx, `DELETE FROM sources WHERE id IN ($1, $2)`, src1.ID, src2.ID)
	})

	// Test A & B: Persistence + Duplicate Ingestion across 2 sources.
	pub := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	fetched := pub.Add(10 * time.Minute)
	artURL := fmt.Sprintf("https://example.com/articles/%s?utm_source=test", suffix)
	art := news.UnifiedArticle{
		SourceID:     src1.ID,
		ExternalID:   "ext-1-" + suffix,
		Title:        "Test Article " + suffix,
		Description:  "Summary",
		OriginalURL:  artURL,
		SourceType:   news.SourceTypeRSS,
		PublishedAt:  &pub,
		DiscoveredAt: fetched,
		FetchedAt:    fetched,
		Language:     domain.LanguageEnglish,
	}

	id1, ins1, err := repo.UpsertArticle(ctx, art)
	if err != nil || !ins1 {
		t.Fatalf("first UpsertArticle() id=%s ins=%v err=%v", id1, ins1, err)
	}
	id2, ins2, err := repo.UpsertArticle(ctx, art)
	if err != nil || ins2 || id2 != id1 {
		t.Fatalf("second UpsertArticle() id=%s ins=%v err=%v (want id=%s ins=false)", id2, ins2, err, id1)
	}

	// Same canonical URL from HN with score/comments.
	score := 99
	comments := 15
	subAt := pub.Add(5 * time.Minute)
	hnArt := news.UnifiedArticle{
		SourceID:          src2.ID,
		ExternalID:        "hn-" + suffix,
		Title:             "Test Article " + suffix,
		OriginalURL:       fmt.Sprintf("https://example.com/articles/%s", suffix),
		SourceType:        news.SourceTypeHackerNews,
		SourceSubmittedAt: &subAt,
		DiscoveredAt:      fetched,
		FetchedAt:         fetched,
		Language:          domain.LanguageEnglish,
		PopularityScore:   &score,
		CommentsCount:     &comments,
	}
	id3, ins3, err := repo.UpsertArticle(ctx, hnArt)
	if err != nil || ins3 || id3 != id1 {
		t.Fatalf("cross-source UpsertArticle() id=%s ins=%v err=%v", id3, ins3, err)
	}

	got, err := repo.GetArticleByExternalID(ctx, src1.ID, art.ExternalID)
	if err != nil {
		t.Fatalf("GetArticleByExternalID() error = %v", err)
	}
	if got.PopularityScore == nil || *got.PopularityScore != 99 {
		t.Fatalf("expected merged HN score 99, got %v", got.PopularityScore)
	}

	// Test D: Parallel Collection (concurrent upserts of the same article).
	parallelURL := fmt.Sprintf("https://example.com/parallel/%s", suffix)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, upErr := repo.UpsertArticle(ctx, news.UnifiedArticle{
				SourceID:     src1.ID,
				ExternalID:   "parallel-" + suffix,
				Title:        "Parallel Article",
				OriginalURL:  parallelURL,
				SourceType:   news.SourceTypeRSS,
				PublishedAt:  &pub,
				DiscoveredAt: fetched,
				FetchedAt:    fetched,
				Language:     domain.LanguageEnglish,
			})
			if upErr != nil {
				errs <- upErr
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent UpsertArticle() error = %v", e)
	}

	// Test F: Daily Snapshot boundaries [start, end).
	msk := domain.Moscow()
	snapEnd := time.Date(2026, 11, 13, 9, 0, 0, 0, msk).UTC()
	snapStart := snapEnd.Add(-24 * time.Hour)
	outsideBefore := snapStart.Add(-1 * time.Minute)
	_, _, err = repo.UpsertArticle(ctx, news.UnifiedArticle{
		SourceID:     src1.ID,
		ExternalID:   "outside-" + suffix,
		Title:        "Outside Window",
		OriginalURL:  fmt.Sprintf("https://example.com/outside/%s", suffix),
		SourceType:   news.SourceTypeRSS,
		PublishedAt:  &outsideBefore,
		DiscoveredAt: fetched,
		FetchedAt:    fetched,
		Language:     domain.LanguageEnglish,
	})
	if err != nil {
		t.Fatalf("insert outside article: %v", err)
	}

	inWindow, err := repo.GetArticlesByTimeRange(ctx, snapStart, snapEnd, false)
	if err != nil {
		t.Fatalf("GetArticlesByTimeRange() error = %v", err)
	}
	var ids []string
	for _, a := range inWindow {
		if a.SourceID == src1.ID {
			ids = append(ids, a.ID.String())
		}
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 src1 articles in window, got %d", len(ids))
	}

	win, _ := domain.NewTimeWindow(snapStart, snapEnd)
	snapDate := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	_, err = repo.CreateDailySnapshot(ctx, postgres.DailySnapshot{
		SnapshotDate:   snapDate,
		Window:         win,
		CoverageStatus: "PARTIAL_COVERAGE",
		SourceStats:    map[string]int{src1.ID: len(ids)},
		WorkflowID:     "test-wf-" + suffix,
	}, ids)
	if err != nil {
		t.Fatalf("CreateDailySnapshot() error = %v", err)
	}

	loadedSnap, err := repo.GetDailySnapshot(ctx, snapDate)
	if err != nil {
		t.Fatalf("GetDailySnapshot() error = %v", err)
	}
	if loadedSnap.ArticleCount != 2 || len(loadedSnap.ArticleIDs) != 2 {
		t.Fatalf("unexpected loaded snapshot: %+v", loadedSnap)
	}
}
