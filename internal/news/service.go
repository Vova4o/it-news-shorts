package news

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// BuildSource constructs a concrete NewsSource adapter from SourceConfig.
func BuildSource(cfg SourceConfig, client *HTTPClient, newsCfg config.NewsConfig) (NewsSource, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Type {
	case SourceTypeHackerNews:
		return NewHackerNewsSource(HackerNewsOptions{
			SourceID:       cfg.ID,
			BaseURL:        cfg.FeedURL,
			Lists:          newsCfg.HNLists,
			LimitPerList:   cfg.Limit,
			MaxConcurrency: newsCfg.MaxConcurrency,
			Language:       cfg.Language,
		}, client)
	case SourceTypeRSS, SourceTypeAtom:
		return NewRSSSource(cfg, client)
	default:
		return nil, fmt.Errorf("news: unknown source type %q", cfg.Type)
	}
}

// DeduplicateArticles removes duplicates across Level 1 (source_id + external_id)
// and Level 2 (canonical_url) while preserving source references and Hacker News
// engagement metrics.
func DeduplicateArticles(articles []UnifiedArticle) ([]UnifiedArticle, int) {
	byExt := make(map[string]int, len(articles))
	byURL := make(map[string]int, len(articles))
	unique := make([]UnifiedArticle, 0, len(articles))
	duplicates := 0

	for _, a := range articles {
		if a.RawMetadata == nil {
			a.RawMetadata = map[string]any{}
		}
		extKey := ""
		if a.SourceID != "" && a.ExternalID != "" {
			extKey = a.SourceID + "\x00" + a.ExternalID
		}
		if extKey != "" {
			if idx, ok := byExt[extKey]; ok {
				unique[idx] = mergeArticle(unique[idx], a)
				duplicates++
				continue
			}
		}
		if a.CanonicalURL != "" {
			if idx, ok := byURL[a.CanonicalURL]; ok {
				unique[idx] = mergeArticle(unique[idx], a)
				if extKey != "" {
					byExt[extKey] = idx
				}
				duplicates++
				continue
			}
		}

		idx := len(unique)
		unique = append(unique, a)
		if extKey != "" {
			byExt[extKey] = idx
		}
		if a.CanonicalURL != "" {
			byURL[a.CanonicalURL] = idx
		}
	}

	return unique, duplicates
}

func mergeArticle(existing, incoming UnifiedArticle) UnifiedArticle {
	if existing.PublishedAt == nil && incoming.PublishedAt != nil {
		t := incoming.PublishedAt.UTC()
		existing.PublishedAt = &t
	}
	if existing.SourceSubmittedAt == nil && incoming.SourceSubmittedAt != nil {
		t := incoming.SourceSubmittedAt.UTC()
		existing.SourceSubmittedAt = &t
	}
	if incoming.PopularityScore != nil {
		if existing.PopularityScore == nil || *incoming.PopularityScore > *existing.PopularityScore {
			v := *incoming.PopularityScore
			existing.PopularityScore = &v
		}
	}
	if incoming.CommentsCount != nil {
		if existing.CommentsCount == nil || *incoming.CommentsCount > *existing.CommentsCount {
			v := *incoming.CommentsCount
			existing.CommentsCount = &v
		}
	}
	if existing.Description == "" && incoming.Description != "" {
		existing.Description = incoming.Description
	}
	if existing.Author == "" && incoming.Author != "" {
		existing.Author = incoming.Author
	}
	if incoming.FetchedAt.After(existing.FetchedAt) {
		existing.FetchedAt = incoming.FetchedAt
	}
	if existing.RawMetadata == nil {
		existing.RawMetadata = map[string]any{}
	}
	sources := make([]string, 0, 2)
	if prev, ok := existing.RawMetadata["discovered_sources"].([]string); ok {
		sources = append(sources, prev...)
	} else if existing.SourceID != "" {
		sources = append(sources, existing.SourceID)
	}
	if incoming.SourceID != "" {
		found := false
		for _, s := range sources {
			if s == incoming.SourceID {
				found = true
				break
			}
		}
		if !found {
			sources = append(sources, incoming.SourceID)
		}
	}
	existing.RawMetadata["discovered_sources"] = sources
	return existing
}

// CollectorService implements NewsCollector across multiple NewsSource adapters.
type CollectorService struct {
	sources        []NewsSource
	maxConcurrency int
	filterOpts     FilterOptions
}

// NewCollectorService constructs a CollectorService.
func NewCollectorService(sources []NewsSource, maxConcurrency int, filterOpts FilterOptions) *CollectorService {
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	return &CollectorService{
		sources:        sources,
		maxConcurrency: maxConcurrency,
		filterOpts:     filterOpts,
	}
}

// Collect fetches all configured sources concurrently, deduplicates, filters by
// window, and converts to domain.Article.
func (c *CollectorService) Collect(ctx context.Context, window domain.TimeWindow) ([]domain.Article, error) {
	if err := window.Validate(); err != nil {
		return nil, err
	}
	now := time.Now().UTC()

	type srcRes struct {
		articles []UnifiedArticle
		err      error
	}
	results := make([]srcRes, len(c.sources))
	sem := make(chan struct{}, c.maxConcurrency)
	var wg sync.WaitGroup

	for i, src := range c.sources {
		wg.Add(1)
		go func(idx int, s NewsSource) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = srcRes{err: ctx.Err()}
				return
			}
			res, err := s.Fetch(ctx, FetchOptions{Now: now})
			results[idx] = srcRes{articles: res.Articles, err: err}
		}(i, src)
	}
	wg.Wait()

	var all []UnifiedArticle
	for _, r := range results {
		if r.err == nil {
			all = append(all, r.articles...)
		}
	}

	deduped, _ := DeduplicateArticles(all)
	accepted, _ := FilterByTimeWindow(deduped, window, now, c.filterOpts)
	out := make([]domain.Article, 0, len(accepted))
	for _, a := range accepted {
		if d, ok := a.ToDomain(); ok {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoArticles
	}
	return out, nil
}
