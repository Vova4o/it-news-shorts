package news

import (
	"context"
	"errors"
	"strings"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// ErrNoArticles is returned when the window produced nothing usable.
var ErrNoArticles = errors.New("news: collector returned no articles")

// NewsCollector fetches IT news for a time window. Implementations arrive in a
// later stage; the domain never depends on them.
type NewsCollector interface {
	Collect(ctx context.Context, window domain.TimeWindow) ([]domain.Article, error)
}

// Source describes one RSS or API source.
type Source struct {
	Name     string
	FeedURL  string
	Language domain.Language
}

// Validate enforces the source rules.
func (s Source) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return domain.MissingField("source.name")
	}
	feed := strings.TrimSpace(s.FeedURL)
	if feed == "" {
		return domain.MissingField("source.feed_url")
	}
	if !strings.HasPrefix(feed, "https://") && !strings.HasPrefix(feed, "http://") {
		return domain.InvalidValue("source.feed_url")
	}
	if err := s.Language.Validate(); err != nil {
		return err
	}
	return nil
}
