package editor_test

import (
	"errors"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
)

var editorBaseTime = time.Date(2026, time.November, 12, 8, 0, 0, 0, time.UTC)

func buildStory(id, title string, score float64, createdAt time.Time) domain.Story {
	return domain.Story{
		ID:         domain.StoryID(id),
		DailyRunID: domain.DailyRunID("run-01"),
		Title:      title,
		Summary:    "Краткое описание новости.",
		Score:      score,
		Evidence: []domain.StoryEvidence{{
			ArticleID:   domain.ArticleID("art-" + id),
			Source:      "example.com",
			URL:         "https://example.com/news/" + id,
			Quote:       "Цитата из источника.",
			PublishedAt: createdAt,
		}},
		CreatedAt: createdAt,
	}
}

func TestSelectStoriesPicksTopThree(t *testing.T) {
	candidates := []domain.Story{
		buildStory("s1", "Новость один", 0.4, editorBaseTime.Add(4*time.Hour)),
		buildStory("s2", "Новость два", 0.9, editorBaseTime.Add(2*time.Hour)),
		buildStory("s3", "Новость три", 0.6, editorBaseTime.Add(3*time.Hour)),
		buildStory("s4", "Новость четыре", 0.75, editorBaseTime.Add(time.Hour)),
		buildStory("s5", "Новость пять", 0.2, editorBaseTime),
	}
	selection, err := editor.SelectStories(candidates, domain.MaxStoriesPerRun)
	if err != nil {
		t.Fatalf("SelectStories() error = %v", err)
	}
	if len(selection.Stories) != domain.MaxStoriesPerRun {
		t.Fatalf("selected %d stories, want %d", len(selection.Stories), domain.MaxStoriesPerRun)
	}
	wantOrder := []domain.StoryID{"s2", "s4", "s3"}
	for i, want := range wantOrder {
		if selection.Stories[i].ID != want {
			t.Fatalf("position %d = %s, want %s", i+1, selection.Stories[i].ID, want)
		}
		if selection.Stories[i].Position != i+1 {
			t.Fatalf("story %s position = %d, want %d", selection.Stories[i].ID, selection.Stories[i].Position, i+1)
		}
	}
	if len(selection.Dropped) != 2 {
		t.Fatalf("dropped %d stories, want 2", len(selection.Dropped))
	}
}

func TestSelectStoriesRemovesDuplicateTitles(t *testing.T) {
	candidates := []domain.Story{
		buildStory("s1", "Go 1.24 Released", 0.9, editorBaseTime),
		buildStory("s2", "  go 1.24   released ", 0.8, editorBaseTime),
		buildStory("s3", "Другая новость", 0.5, editorBaseTime),
	}
	selection, err := editor.SelectStories(candidates, domain.MaxStoriesPerRun)
	if err != nil {
		t.Fatalf("SelectStories() error = %v", err)
	}
	if len(selection.Stories) != 2 {
		t.Fatalf("selected %d stories, want 2", len(selection.Stories))
	}
	if selection.Stories[0].ID != domain.StoryID("s1") {
		t.Fatalf("first story = %s, want s1", selection.Stories[0].ID)
	}
}

func TestSelectStoriesFewerCandidatesThanLimit(t *testing.T) {
	candidates := []domain.Story{buildStory("s1", "Одна новость", 0.5, editorBaseTime)}
	selection, err := editor.SelectStories(candidates, domain.MaxStoriesPerRun)
	if err != nil {
		t.Fatalf("SelectStories() error = %v", err)
	}
	if len(selection.Stories) != 1 {
		t.Fatalf("selected %d stories, want 1", len(selection.Stories))
	}
	if len(selection.Dropped) != 0 {
		t.Fatalf("dropped %d stories, want 0", len(selection.Dropped))
	}
}

func TestSelectStoriesRejectsInvalidLimit(t *testing.T) {
	candidates := []domain.Story{buildStory("s1", "Новость", 0.5, editorBaseTime)}
	for _, limit := range []int{0, -1, domain.MaxStoriesPerRun + 1} {
		if _, err := editor.SelectStories(candidates, limit); !errors.Is(err, domain.ErrTooManyStories) {
			t.Fatalf("limit %d: error = %v, want %v", limit, err, domain.ErrTooManyStories)
		}
	}
}

func TestSelectStoriesRejectsEmptyInput(t *testing.T) {
	if _, err := editor.SelectStories(nil, domain.MaxStoriesPerRun); !errors.Is(err, editor.ErrNoCandidates) {
		t.Fatalf("error = %v, want %v", err, editor.ErrNoCandidates)
	}
}

func TestSelectStoriesRejectsUnverifiedStory(t *testing.T) {
	broken := buildStory("s1", "Новость без источника", 0.5, editorBaseTime)
	broken.Evidence = nil
	if _, err := editor.SelectStories([]domain.Story{broken}, domain.MaxStoriesPerRun); !errors.Is(err, domain.ErrMissingField) {
		t.Fatalf("error = %v, want %v", err, domain.ErrMissingField)
	}
}
