package news

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

const hnDiscussionURLFormat = "https://news.ycombinator.com/item?id=%d"

// HackerNewsOptions configures the Hacker News API adapter.
type HackerNewsOptions struct {
	SourceID       string
	BaseURL        string
	Lists          []string // e.g. ["top", "new", "best"]
	LimitPerList   int
	MaxConcurrency int
	Language       domain.Language
}

// HackerNewsSource fetches stories from the official Hacker News Firebase API.
type HackerNewsSource struct {
	sourceID       string
	baseURL        string
	lists          []string
	limitPerList   int
	maxConcurrency int
	language       domain.Language
	client         *HTTPClient
}

type hnItem struct {
	ID          int64  `json:"id"`
	Deleted     bool   `json:"deleted"`
	Dead        bool   `json:"dead"`
	Type        string `json:"type"`
	By          string `json:"by"`
	Time        int64  `json:"time"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	URL         string `json:"url"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
}

// NewHackerNewsSource creates a Hacker News NewsSource adapter.
func NewHackerNewsSource(opts HackerNewsOptions, client *HTTPClient) (*HackerNewsSource, error) {
	if client == nil {
		return nil, fmt.Errorf("news: http client is required")
	}
	sourceID := strings.TrimSpace(opts.SourceID)
	if sourceID == "" {
		sourceID = "hackernews"
	}
	baseURL := strings.TrimRight(strings.TrimSpace(opts.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://hacker-news.firebaseio.com/v0"
	}
	lists := opts.Lists
	if len(lists) == 0 {
		lists = []string{"top", "new", "best"}
	}
	limit := opts.LimitPerList
	if limit <= 0 {
		limit = 30
	}
	conc := opts.MaxConcurrency
	if conc <= 0 {
		conc = 8
	}
	lang := opts.Language
	if lang == "" {
		lang = domain.LanguageEnglish
	}
	if err := lang.Validate(); err != nil {
		return nil, err
	}

	return &HackerNewsSource{
		sourceID:       sourceID,
		baseURL:        baseURL,
		lists:          lists,
		limitPerList:   limit,
		maxConcurrency: conc,
		language:       lang,
		client:         client,
	}, nil
}

// ID returns the configured source identifier.
func (s *HackerNewsSource) ID() string {
	return s.sourceID
}

// Fetch retrieves story IDs from the configured HN lists, deduplicates them,
// and fetches individual items with bounded concurrency.
func (s *HackerNewsSource) Fetch(ctx context.Context, opts FetchOptions) (FetchResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = s.limitPerList
	}
	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	ids, err := s.fetchUniqueIDs(ctx, limit)
	if err != nil {
		return FetchResult{}, err
	}
	if len(ids) == 0 {
		return FetchResult{}, nil
	}

	type itemResult struct {
		idx     int
		article *UnifiedArticle
		err     error
	}

	results := make([]itemResult, len(ids))
	sem := make(chan struct{}, s.maxConcurrency)
	var wg sync.WaitGroup

	for i, id := range ids {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int, itemID int64) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[idx] = itemResult{idx: idx, err: ctx.Err()}
				return
			}
			art, fetchErr := s.fetchItem(ctx, itemID, now)
			results[idx] = itemResult{idx: idx, article: art, err: fetchErr}
		}(i, id)
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return FetchResult{}, err
	}

	articles := make([]UnifiedArticle, 0, len(ids))
	var firstFatal error
	fetchedCount := 0
	for _, r := range results {
		if r.err != nil {
			if firstFatal == nil {
				firstFatal = r.err
			}
			continue
		}
		if r.article != nil {
			fetchedCount++
			articles = append(articles, *r.article)
		}
	}

	if len(articles) == 0 && firstFatal != nil {
		return FetchResult{}, firstFatal
	}

	return FetchResult{
		Articles:     articles,
		FetchedCount: fetchedCount,
	}, nil
}

func (s *HackerNewsSource) fetchUniqueIDs(ctx context.Context, limitPerList int) ([]int64, error) {
	seen := make(map[int64]struct{})
	var unique []int64
	var listErrs []error

	for _, listName := range s.lists {
		endpoint, err := hnListEndpoint(listName)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.Get(ctx, s.baseURL+endpoint, "", "")
		if err != nil {
			listErrs = append(listErrs, err)
			continue
		}
		var ids []int64
		if err := json.Unmarshal(resp.Body, &ids); err != nil {
			listErrs = append(listErrs, fmt.Errorf("news: decode hackernews %s list: %w", listName, err))
			continue
		}
		count := 0
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			if count >= limitPerList {
				break
			}
			count++
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}

	if len(unique) == 0 && len(listErrs) > 0 {
		return nil, errors.Join(listErrs...)
	}
	return unique, nil
}

func hnListEndpoint(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "top", "topstories":
		return "/topstories.json", nil
	case "new", "newstories":
		return "/newstories.json", nil
	case "best", "beststories":
		return "/beststories.json", nil
	default:
		return "", fmt.Errorf("news: unknown hackernews list %q", name)
	}
}

func (s *HackerNewsSource) fetchItem(ctx context.Context, id int64, now time.Time) (*UnifiedArticle, error) {
	itemURL := fmt.Sprintf("%s/item/%d.json", s.baseURL, id)
	resp, err := s.client.Get(ctx, itemURL, "", "")
	if err != nil {
		var upstream *UpstreamError
		if errors.As(err, &upstream) && upstream.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	trimmed := bytes.TrimSpace(resp.Body)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}

	var item hnItem
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return nil, fmt.Errorf("news: decode hackernews item %d: %w", id, err)
	}

	if item.ID <= 0 || item.Deleted || item.Dead {
		return nil, nil
	}
	if item.Type != "story" {
		return nil, nil
	}
	if strings.TrimSpace(item.Title) == "" {
		return nil, nil
	}

	discussionURL := fmt.Sprintf(hnDiscussionURLFormat, item.ID)
	originalURL := strings.TrimSpace(item.URL)
	if originalURL == "" {
		originalURL = discussionURL
	}

	var submittedAt *time.Time
	if item.Time > 0 {
		t := time.Unix(item.Time, 0).UTC()
		submittedAt = &t
	}
	score := item.Score
	comments := item.Descendants

	raw := UnifiedArticle{
		SourceID:          s.sourceID,
		ExternalID:        strconv.FormatInt(item.ID, 10),
		Title:             item.Title,
		Description:       item.Text,
		OriginalURL:       originalURL,
		SourceType:        SourceTypeHackerNews,
		Author:            item.By,
		PublishedAt:       nil, // HN API only reports HN submission time, not original article publication time.
		SourceSubmittedAt: submittedAt,
		DiscoveredAt:      now,
		FetchedAt:         now,
		Language:          s.language,
		PopularityScore:   &score,
		CommentsCount:     &comments,
		RawMetadata: map[string]any{
			"hn_id":  item.ID,
			"hn_url": discussionURL,
			"type":   item.Type,
		},
	}

	norm, err := NormalizeArticle(raw)
	if err != nil {
		return nil, nil
	}
	return &norm, nil
}
