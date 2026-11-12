package ai

import (
	"context"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// RankedStory is one ranking decision of the LLM.
type RankedStory struct {
	StoryID domain.StoryID
	Score   float64
	Reason  string
}

// Validate enforces the ranking rules.
func (r RankedStory) Validate() error {
	if r.StoryID == "" {
		return domain.MissingField("ranked_story.story_id")
	}
	if r.Score < 0 || r.Score > 1 {
		return domain.InvalidValue("ranked_story.score")
	}
	return nil
}

// StoryRanker orders news candidates by short-form potential.
type StoryRanker interface {
	Rank(ctx context.Context, candidates []domain.Story) ([]RankedStory, error)
}

// FactCheckResult is the verdict of the fact checking step.
type FactCheckResult struct {
	StoryID    domain.StoryID
	Verified   bool
	Confidence float64
	Notes      []string
	CheckedAt  time.Time
}

// Validate enforces the fact check rules.
func (r FactCheckResult) Validate() error {
	if r.StoryID == "" {
		return domain.MissingField("fact_check.story_id")
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return domain.InvalidValue("fact_check.confidence")
	}
	if r.CheckedAt.IsZero() {
		return domain.MissingField("fact_check.checked_at")
	}
	if !r.Verified && len(r.Notes) == 0 {
		return domain.MissingField("fact_check.notes")
	}
	return nil
}

// FactChecker verifies claims against independent sources.
type FactChecker interface {
	Check(ctx context.Context, story domain.Story) (FactCheckResult, error)
}
