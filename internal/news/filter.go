package news

import (
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Exclusion reason constants for time-window filtering.
const (
	ExcludeMissingPublishedAt = "missing_published_at"
	ExcludeFuturePublishedAt  = "future_published_at"
	ExcludeBeforeWindow       = "before_window"
	ExcludeAfterWindow        = "after_window"
)

// FilterOptions configures time-window filtering behavior.
type FilterOptions struct {
	MaxFutureSkew           time.Duration
	UseSubmittedAtIfMissing bool
}

// FilterByTimeWindow partitions articles into accepted and excluded slices
// according to the half-open interval [window.Start, window.End).
// When PublishedAt is present, it is always used for freshness even if
// SourceSubmittedAt differs (e.g. Hacker News submissions of older articles).
func FilterByTimeWindow(
	articles []UnifiedArticle,
	window domain.TimeWindow,
	now time.Time,
	opts FilterOptions,
) (accepted []UnifiedArticle, excluded []UnifiedArticle) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	skew := opts.MaxFutureSkew
	if skew < 0 {
		skew = 0
	}

	for _, a := range articles {
		ts := a.PublishedAt
		if (ts == nil || ts.IsZero()) && opts.UseSubmittedAtIfMissing && a.SourceSubmittedAt != nil && !a.SourceSubmittedAt.IsZero() {
			ts = a.SourceSubmittedAt
		}
		if ts == nil || ts.IsZero() {
			a.ExclusionReason = ExcludeMissingPublishedAt
			excluded = append(excluded, a)
			continue
		}

		utc := ts.UTC()
		if utc.After(now.Add(skew)) {
			a.ExclusionReason = ExcludeFuturePublishedAt
			excluded = append(excluded, a)
			continue
		}
		if utc.Before(window.Start.UTC()) {
			a.ExclusionReason = ExcludeBeforeWindow
			excluded = append(excluded, a)
			continue
		}
		if !utc.Before(window.End.UTC()) {
			a.ExclusionReason = ExcludeAfterWindow
			excluded = append(excluded, a)
			continue
		}

		a.ExclusionReason = ""
		accepted = append(accepted, a)
	}
	return accepted, excluded
}
