package editor

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// ErrNoCandidates is returned when a run has nothing to choose from.
var ErrNoCandidates = errors.New("editor: no story candidates")

// Selection is the outcome of the editorial decision.
type Selection struct {
	Stories []domain.Story
	Dropped []domain.Story
}

// SelectStories picks at most limit verified stories: highest score first,
// older stories first on a tie, duplicate headlines removed. Positions are
// assigned from one.
func SelectStories(candidates []domain.Story, limit int) (Selection, error) {
	if limit < 1 || limit > domain.MaxStoriesPerRun {
		return Selection{}, fmt.Errorf("%w: limit %d", domain.ErrTooManyStories, limit)
	}
	if len(candidates) == 0 {
		return Selection{}, ErrNoCandidates
	}

	seen := make(map[string]struct{}, len(candidates))
	unique := make([]domain.Story, 0, len(candidates))
	for _, story := range candidates {
		if err := story.Validate(); err != nil {
			return Selection{}, fmt.Errorf("editor: invalid candidate: %w", err)
		}
		key := normalizeTitle(story.Title)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, story)
	}

	sort.SliceStable(unique, func(i, k int) bool {
		if unique[i].Score != unique[k].Score {
			return unique[i].Score > unique[k].Score
		}
		return unique[i].CreatedAt.Before(unique[k].CreatedAt)
	})

	if limit > len(unique) {
		limit = len(unique)
	}
	selected := make([]domain.Story, 0, limit)
	for i := 0; i < limit; i++ {
		story := unique[i]
		story.Position = i + 1
		selected = append(selected, story)
	}
	dropped := append([]domain.Story(nil), unique[limit:]...)
	return Selection{Stories: selected, Dropped: dropped}, nil
}

func normalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}
