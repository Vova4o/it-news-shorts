package news

import (
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func TestDeduplicateArticles(t *testing.T) {
	pub := time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)
	sub := time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)
	score := 250
	comments := 80

	items := []UnifiedArticle{
		{
			SourceID:          "hackernews",
			ExternalID:        "100",
			Title:             "OpenAI releases new model",
			OriginalURL:       "https://techcrunch.com/2026/11/13/model/?utm_source=hn",
			CanonicalURL:      "https://techcrunch.com/2026/11/13/model/",
			SourceSubmittedAt: &sub,
			PopularityScore:   &score,
			CommentsCount:     &comments,
		},
		// Same article from TechCrunch RSS (has PublishedAt, same CanonicalURL).
		{
			SourceID:     "techcrunch",
			ExternalID:   "tc-1",
			Title:        "OpenAI releases new model",
			OriginalURL:  "https://techcrunch.com/2026/11/13/model/",
			CanonicalURL: "https://techcrunch.com/2026/11/13/model/",
			PublishedAt:  &pub,
		},
		// Duplicate external ID on techcrunch.
		{
			SourceID:     "techcrunch",
			ExternalID:   "tc-1",
			Title:        "OpenAI releases new model",
			OriginalURL:  "https://techcrunch.com/2026/11/13/model/",
			CanonicalURL: "https://techcrunch.com/2026/11/13/model/",
			PublishedAt:  &pub,
		},
		// Different article with similar title from The Verge (must NOT be auto-merged).
		{
			SourceID:     "theverge",
			ExternalID:   "tv-2",
			Title:        "OpenAI releases new model today",
			OriginalURL:  "https://www.theverge.com/2026/11/13/openai-model",
			CanonicalURL: "https://www.theverge.com/2026/11/13/openai-model",
			PublishedAt:  &pub,
		},
	}

	unique, dupes := DeduplicateArticles(items)
	if len(unique) != 2 {
		t.Fatalf("len(unique) = %d, want 2", len(unique))
	}
	if dupes != 2 {
		t.Fatalf("dupes = %d, want 2", dupes)
	}
	if unique[0].PublishedAt == nil || !unique[0].PublishedAt.Equal(pub) {
		t.Fatalf("expected PublishedAt merged from TechCrunch, got %v", unique[0].PublishedAt)
	}
	if unique[0].PopularityScore == nil || *unique[0].PopularityScore != 250 {
		t.Fatalf("expected HN score preserved, got %v", unique[0].PopularityScore)
	}
	if sim := TitleSimilarity(unique[0].Title, unique[1].Title); sim <= 0.5 {
		t.Fatalf("expected high TitleSimilarity, got %f", sim)
	}
}

func TestFilterByTimeWindow_HalfOpenAndHNDate(t *testing.T) {
	msk := domain.Moscow()
	endMSK := time.Date(2026, 11, 13, 9, 0, 0, 0, msk)
	startMSK := endMSK.Add(-24 * time.Hour)
	win, err := domain.NewTimeWindow(startMSK.UTC(), endMSK.UTC())
	if err != nil {
		t.Fatalf("NewTimeWindow() error = %v", err)
	}
	now := endMSK.UTC()

	atStart := startMSK.UTC()
	beforeStart := startMSK.Add(-1 * time.Second).UTC()
	beforeEnd := endMSK.Add(-1 * time.Second).UTC()
	atEnd := endMSK.UTC()
	future := endMSK.Add(2 * time.Hour).UTC()
	oldArticlePub := startMSK.Add(-48 * time.Hour).UTC()
	recentHNSub := endMSK.Add(-2 * time.Hour).UTC()

	articles := []UnifiedArticle{
		{ID: "exact-start", PublishedAt: &atStart},
		{ID: "1s-before-start", PublishedAt: &beforeStart},
		{ID: "1s-before-end", PublishedAt: &beforeEnd},
		{ID: "exact-end", PublishedAt: &atEnd},
		{ID: "missing-date", PublishedAt: nil},
		{ID: "future-date", PublishedAt: &future},
		{ID: "hn-old-article-recent-sub", PublishedAt: &oldArticlePub, SourceSubmittedAt: &recentHNSub},
	}

	accepted, excluded := FilterByTimeWindow(articles, win, now, FilterOptions{MaxFutureSkew: 5 * time.Minute})
	if len(accepted) != 2 {
		t.Fatalf("len(accepted) = %d, want 2 (%+v)", len(accepted), accepted)
	}
	if accepted[0].ID != "exact-start" || accepted[1].ID != "1s-before-end" {
		t.Fatalf("unexpected accepted IDs: %+v", accepted)
	}

	reasons := make(map[domain.ArticleID]string, len(excluded))
	for _, ex := range excluded {
		reasons[ex.ID] = ex.ExclusionReason
	}
	if reasons["1s-before-start"] != ExcludeBeforeWindow {
		t.Fatalf("1s-before-start reason = %q", reasons["1s-before-start"])
	}
	if reasons["exact-end"] != ExcludeAfterWindow {
		t.Fatalf("exact-end reason = %q", reasons["exact-end"])
	}
	if reasons["missing-date"] != ExcludeMissingPublishedAt {
		t.Fatalf("missing-date reason = %q", reasons["missing-date"])
	}
	if reasons["future-date"] != ExcludeFuturePublishedAt {
		t.Fatalf("future-date reason = %q", reasons["future-date"])
	}
	if reasons["hn-old-article-recent-sub"] != ExcludeBeforeWindow {
		t.Fatalf("hn-old-article-recent-sub reason = %q, want %q", reasons["hn-old-article-recent-sub"], ExcludeBeforeWindow)
	}
}
