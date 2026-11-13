package editor

import (
	"fmt"
	"sort"
)

// DailyStorySelector chooses up to maxStories verified, non-duplicate stories
// with category diversity enforcement.
type DailyStorySelector struct {
	maxStories int
}

// NewDailyStorySelector constructs a DailyStorySelector.
func NewDailyStorySelector(maxStories int) *DailyStorySelector {
	if maxStories < 1 || maxStories > 3 {
		maxStories = 3
	}
	return &DailyStorySelector{maxStories: maxStories}
}

// SelectionResult contains the selected TOP-N items and rejected story reasons.
type SelectionResult struct {
	Selected []SelectedStoryItem
	Rejected []ClusteredStory
}

// Select picks up to maxStories verified, non-repeated stories sorted by
// FinalScore descending, enforcing a max-2-per-category diversity rule when
// alternatives exist. Never fabricates stories when fewer than maxStories qualify.
func (s *DailyStorySelector) Select(
	candidates []ClusteredStory,
	previouslyPublishedFingerprints map[string]bool,
) SelectionResult {
	var eligible []ClusteredStory
	var rejected []ClusteredStory

	for _, c := range candidates {
		if !c.VerificationPassed || c.Score == nil {
			if c.RejectionReason == "" {
				c.RejectionReason = "failed fact verification or missing score"
			}
			rejected = append(rejected, c)
			continue
		}
		if previouslyPublishedFingerprints[c.Fingerprint] && !c.IsSignificantUpdate {
			c.RejectionReason = "already published in a previous selection"
			rejected = append(rejected, c)
			continue
		}
		eligible = append(eligible, c)
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].Score.FinalScore != eligible[j].Score.FinalScore {
			return eligible[i].Score.FinalScore > eligible[j].Score.FinalScore
		}
		if !eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
		}
		return eligible[i].ID < eligible[j].ID
	})

	if len(eligible) == 0 {
		return SelectionResult{Rejected: rejected}
	}

	const maxPerCategory = 2
	catCounts := make(map[StoryCategory]int)
	pickedIdx := make(map[int]bool)
	var chosen []ClusteredStory

	// Pass 1: respect category diversity (<= 2 per category).
	for i, c := range eligible {
		if len(chosen) >= s.maxStories {
			break
		}
		if catCounts[c.Category] >= maxPerCategory {
			continue
		}
		catCounts[c.Category]++
		pickedIdx[i] = true
		chosen = append(chosen, c)
	}

	// Pass 2: if fewer than maxStories were chosen due to diversity cap,
	// backfill with remaining verified stories and document the exception.
	diversityOverride := false
	if len(chosen) < s.maxStories {
		for i, c := range eligible {
			if len(chosen) >= s.maxStories {
				break
			}
			if pickedIdx[i] {
				continue
			}
			diversityOverride = true
			pickedIdx[i] = true
			chosen = append(chosen, c)
		}
	}

	for i, c := range eligible {
		if !pickedIdx[i] {
			c.RejectionReason = fmt.Sprintf("not in top %d by final score (%.2f)", s.maxStories, c.Score.FinalScore)
			rejected = append(rejected, c)
		}
	}

	selected := make([]SelectedStoryItem, 0, len(chosen))
	for i, c := range chosen {
		var claimIDs []string
		for _, cl := range c.Claims {
			if cl.VerificationStatus == StatusVerified {
				claimIDs = append(claimIDs, cl.ID)
			}
		}
		seenURL := make(map[string]struct{}, len(c.Articles))
		var sourceURLs []string
		for _, a := range c.Articles {
			u := a.CanonicalURL
			if u == "" {
				u = a.OriginalURL
			}
			if u != "" {
				if _, ok := seenURL[u]; !ok {
					seenURL[u] = struct{}{}
					sourceURLs = append(sourceURLs, u)
				}
			}
		}
		reason := c.Score.ReasoningSummary
		if diversityOverride && catCounts[c.Category] >= maxPerCategory {
			reason += " (selected as best remaining verified story; no higher-scoring alternative category available)"
		}
		selected = append(selected, SelectedStoryItem{
			Rank:             i + 1,
			StoryID:          c.ID.String(),
			Title:            c.Title,
			Category:         string(c.Category),
			Score:            c.Score.FinalScore,
			VerifiedClaimIDs: claimIDs,
			SourceURLs:       sourceURLs,
			SelectionReason:  reason,
			Fingerprint:      c.Fingerprint,
		})
	}

	return SelectionResult{
		Selected: selected,
		Rejected: rejected,
	}
}
