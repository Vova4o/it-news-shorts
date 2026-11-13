package news

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// SourceType identifies the feed protocol of a news source.
type SourceType string

const (
	SourceTypeRSS        SourceType = "rss"
	SourceTypeAtom       SourceType = "atom"
	SourceTypeHackerNews SourceType = "hackernews"
)

// SourceConfig describes a configured news source.
type SourceConfig struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Type     SourceType      `json:"type"`
	FeedURL  string          `json:"feed_url"`
	Language domain.Language `json:"language"`
	Enabled  bool            `json:"enabled"`
	Limit    int             `json:"limit,omitempty"`
}

// Validate checks that a SourceConfig has all required fields.
func (s SourceConfig) Validate() error {
	if strings.TrimSpace(s.ID) == "" {
		return domain.MissingField("source.id")
	}
	if strings.TrimSpace(s.Name) == "" {
		return domain.MissingField("source.name")
	}
	if s.Type != SourceTypeRSS && s.Type != SourceTypeAtom && s.Type != SourceTypeHackerNews {
		return domain.InvalidValue("source.type")
	}
	if strings.TrimSpace(s.FeedURL) == "" {
		return domain.MissingField("source.feed_url")
	}
	if !strings.HasPrefix(s.FeedURL, "http://") && !strings.HasPrefix(s.FeedURL, "https://") {
		return domain.InvalidValue("source.feed_url")
	}
	if err := s.Language.Validate(); err != nil {
		return err
	}
	return nil
}

// FetchOptions controls a single source fetch call.
type FetchOptions struct {
	ETag         string
	LastModified string
	Limit        int
	Now          time.Time
}

// FetchResult is the outcome of fetching one NewsSource.
type FetchResult struct {
	Articles     []UnifiedArticle
	NotModified  bool
	ETag         string
	LastModified string
	FetchedCount int
}

// NewsSource fetches and parses raw news items from one external source.
type NewsSource interface {
	ID() string
	Fetch(ctx context.Context, opts FetchOptions) (FetchResult, error)
}

// UnifiedArticle is the normalized, source-agnostic representation of a
// collected news item. Missing optional values are represented explicitly with
// nil pointers rather than fabricated defaults.
type UnifiedArticle struct {
	ID                domain.ArticleID
	SourceID          string
	ExternalID        string
	Title             string
	NormalizedTitle   string
	Description       string
	OriginalURL       string
	CanonicalURL      string
	SourceType        SourceType
	Author            string
	PublishedAt       *time.Time
	SourceSubmittedAt *time.Time
	DiscoveredAt      time.Time
	FetchedAt         time.Time
	Language          domain.Language
	Categories        []string
	PopularityScore   *int
	CommentsCount     *int
	RawMetadata       map[string]any
	ExclusionReason   string
}

// EffectivePublishedAt returns PublishedAt when known, or nil if absent.
func (a UnifiedArticle) EffectivePublishedAt() *time.Time {
	return a.PublishedAt
}

// ToDomain converts a UnifiedArticle into a domain.Article when PublishedAt is
// available.
func (a UnifiedArticle) ToDomain() (domain.Article, bool) {
	if a.PublishedAt == nil || a.PublishedAt.IsZero() {
		return domain.Article{}, false
	}
	urlStr := a.CanonicalURL
	if urlStr == "" {
		urlStr = a.OriginalURL
	}
	return domain.Article{
		ID:          a.ID,
		URL:         urlStr,
		Title:       a.Title,
		Summary:     a.Description,
		Source:      a.SourceID,
		Language:    a.Language,
		PublishedAt: a.PublishedAt.UTC(),
		FetchedAt:   a.FetchedAt.UTC(),
	}, true
}

// DeterministicArticleID builds a stable, collision-resistant ArticleID from
// canonical URL or (sourceID, externalID).
func DeterministicArticleID(sourceID, externalID, canonicalURL string) domain.ArticleID {
	key := canonicalURL
	if key == "" {
		key = sourceID + ":" + externalID
	}
	sum := sha256.Sum256([]byte(key))
	return domain.ArticleID("art-" + hex.EncodeToString(sum[:16]))
}

// DefaultSources returns the built-in IT news sources (Hacker News + TechCrunch,
// The Verge, Ars Technica) merged with any extra RSS sources from config.
func DefaultSources(cfg config.NewsConfig) ([]SourceConfig, error) {
	hnURL := cfg.HNBaseURL
	if hnURL == "" {
		hnURL = "https://hacker-news.firebaseio.com/v0"
	}
	rssLimit := cfg.RSSLimit
	if rssLimit <= 0 {
		rssLimit = 50
	}
	hnLimit := cfg.HNLimit
	if hnLimit <= 0 {
		hnLimit = 30
	}

	sources := []SourceConfig{
		{
			ID:       "hackernews",
			Name:     "Hacker News",
			Type:     SourceTypeHackerNews,
			FeedURL:  hnURL,
			Language: domain.LanguageEnglish,
			Enabled:  true,
			Limit:    hnLimit,
		},
		{
			ID:       "techcrunch",
			Name:     "TechCrunch",
			Type:     SourceTypeRSS,
			FeedURL:  "https://techcrunch.com/feed/",
			Language: domain.LanguageEnglish,
			Enabled:  true,
			Limit:    rssLimit,
		},
		{
			ID:       "theverge",
			Name:     "The Verge",
			Type:     SourceTypeAtom,
			FeedURL:  "https://www.theverge.com/rss/index.xml",
			Language: domain.LanguageEnglish,
			Enabled:  true,
			Limit:    rssLimit,
		},
		{
			ID:       "arstechnica",
			Name:     "Ars Technica",
			Type:     SourceTypeRSS,
			FeedURL:  "https://feeds.arstechnica.com/arstechnica/index",
			Language: domain.LanguageEnglish,
			Enabled:  true,
			Limit:    rssLimit,
		},
	}

	if strings.TrimSpace(cfg.ExtraRSSSources) != "" {
		var extra []SourceConfig
		if err := json.Unmarshal([]byte(cfg.ExtraRSSSources), &extra); err != nil {
			return nil, fmt.Errorf("news: parse NEWS_EXTRA_RSS_SOURCES: %w", err)
		}
		seen := make(map[string]int, len(sources))
		for i, s := range sources {
			seen[s.ID] = i
		}
		for _, s := range extra {
			if s.Type == "" {
				s.Type = SourceTypeRSS
			}
			if s.Language == "" {
				s.Language = domain.LanguageEnglish
			}
			if s.Limit <= 0 {
				s.Limit = rssLimit
			}
			if err := s.Validate(); err != nil {
				return nil, err
			}
			if idx, ok := seen[s.ID]; ok {
				sources[idx] = s
			} else {
				seen[s.ID] = len(sources)
				sources = append(sources, s)
			}
		}
	}

	return sources, nil
}
