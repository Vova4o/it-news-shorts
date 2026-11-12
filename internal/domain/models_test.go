package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func validArticle() domain.Article {
	return domain.Article{
		ID:          domain.ArticleID("art-01"),
		URL:         "https://example.com/news/go-1-24",
		Title:       "Go 1.24 released",
		Summary:     "The Go team shipped a new release.",
		Source:      "example.com",
		Language:    domain.LanguageEnglish,
		PublishedAt: baseTime,
		FetchedAt:   baseTime.Add(time.Hour),
	}
}

func validEvidence() domain.StoryEvidence {
	article := validArticle()
	return domain.StoryEvidence{
		ArticleID:   article.ID,
		Source:      article.Source,
		URL:         article.URL,
		Quote:       "Go 1.24 is available today.",
		PublishedAt: article.PublishedAt,
	}
}

func validStory() domain.Story {
	return domain.Story{
		ID:         domain.StoryID("story-01"),
		DailyRunID: domain.DailyRunID("run-01"),
		Title:      "Go 1.24 released",
		Summary:    "A new Go release.",
		Score:      0.8,
		Position:   1,
		Evidence:   []domain.StoryEvidence{validEvidence()},
		CreatedAt:  baseTime,
	}
}

func validScript() domain.Script {
	return domain.Script{
		ID:                domain.ScriptID("script-01"),
		VideoJobID:        domain.VideoJobID("vj-01"),
		Language:          domain.LanguageRussian,
		Narration:         "Готовьтесь: вышла новая версия Go.",
		EstimatedDuration: domain.ShortsDuration(22 * time.Second),
		Version:           1,
		CreatedAt:         baseTime,
	}
}

func validMediaAsset() domain.MediaAsset {
	return domain.MediaAsset{
		ID:         domain.MediaAssetID("asset-01"),
		VideoJobID: domain.VideoJobID("vj-01"),
		Kind:       domain.MediaRendered,
		URI:        "s3://media/vj-01/final.mp4",
		MIMEType:   "video/mp4",
		SizeBytes:  1024,
		Duration:   22 * time.Second,
		Checksum:   "sha256:abc",
		Provider:   "ffmpeg",
		CreatedAt:  baseTime,
	}
}

func validPublication() domain.Publication {
	return domain.Publication{
		ID:             domain.PublicationID("pub-01"),
		VideoJobID:     domain.VideoJobID("vj-01"),
		YouTubeVideoID: "dQw4w9WgXcQ",
		Status:         domain.PublicationPublished,
		IdempotencyKey: domain.IdempotencyKey("run-01/story-01/publish"),
		ScheduledAt:    baseTime,
		PublishedAt:    baseTime.Add(time.Hour),
		CreatedAt:      baseTime,
	}
}

func validCost() domain.GenerationCost {
	return domain.GenerationCost{
		ID:         domain.CostID("cost-01"),
		VideoJobID: domain.VideoJobID("vj-01"),
		Provider:   "llm",
		Operation:  "script",
		Units:      2,
		UnitCost:   domain.MicroUSD(500),
		Total:      domain.MicroUSD(1000),
		CreatedAt:  baseTime,
	}
}

func validDailyRun() domain.DailyRun {
	return domain.DailyRun{
		ID:      domain.DailyRunID("run-01"),
		RunDate: baseTime,
		Window: domain.TimeWindow{
			Start: baseTime.Add(-24 * time.Hour),
			End:   baseTime,
		},
		State:      domain.DailyRunPending,
		MaxStories: domain.MaxStoriesPerRun,
		CreatedAt:  baseTime,
	}
}

func TestValidModelsPass(t *testing.T) {
	checks := []struct {
		name     string
		validate func() error
	}{
		{"article", func() error { return validArticle().Validate() }},
		{"evidence", func() error { return validEvidence().Validate() }},
		{"story", func() error { return validStory().Validate() }},
		{"unselected story has zero position", func() error {
			story := validStory()
			story.Position = 0
			return story.Validate()
		}},
		{"story at max position", func() error {
			story := validStory()
			story.Position = domain.MaxStoriesPerRun
			return story.Validate()
		}},
		{"daily run", func() error { return validDailyRun().Validate() }},
		{"script", func() error { return validScript().Validate() }},
		{"media asset", func() error { return validMediaAsset().Validate() }},
		{"publication", func() error { return validPublication().Validate() }},
		{"cost", func() error { return validCost().Validate() }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestModelValidation(t *testing.T) {
	tests := []struct {
		name      string
		validate  func() error
		wantErr   error
		wantField string
	}{
		{
			name: "article without title",
			validate: func() error {
				article := validArticle()
				article.Title = ""
				return article.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "article.title",
		},
		{
			name: "article without source",
			validate: func() error {
				article := validArticle()
				article.Source = ""
				return article.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "article.source",
		},
		{
			name: "article with bad url",
			validate: func() error {
				article := validArticle()
				article.URL = "not-a-url"
				return article.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "article.url",
		},
		{
			name: "article fetched before publication",
			validate: func() error {
				article := validArticle()
				article.FetchedAt = article.PublishedAt.Add(-time.Minute)
				return article.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "article.fetched_at",
		},
		{
			name: "evidence without quote",
			validate: func() error {
				evidence := validEvidence()
				evidence.Quote = ""
				return evidence.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "evidence.quote",
		},
		{
			name: "story without evidence",
			validate: func() error {
				story := validStory()
				story.Evidence = nil
				return story.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "story.evidence",
		},
		{
			name: "story with score above one",
			validate: func() error {
				story := validStory()
				story.Score = 1.5
				return story.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "story.score",
		},
		{
			name: "story with position above limit",
			validate: func() error {
				story := validStory()
				story.Position = domain.MaxStoriesPerRun + 1
				return story.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "story.position",
		},
		{
			name: "story with negative position",
			validate: func() error {
				story := validStory()
				story.Position = -1
				return story.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "story.position",
		},
		{
			name: "daily run with too many stories",
			validate: func() error {
				run := validDailyRun()
				run.MaxStories = domain.MaxStoriesPerRun + 1
				return run.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "daily_run.max_stories",
		},
		{
			name: "daily run with short window",
			validate: func() error {
				run := validDailyRun()
				run.Window.End = run.Window.Start.Add(23 * time.Hour)
				return run.Validate()
			},
			wantErr:   domain.ErrInvalidWindow,
			wantField: "window.duration",
		},
		{
			name: "script without narration",
			validate: func() error {
				script := validScript()
				script.Narration = "   "
				return script.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "script.narration",
		},
		{
			name: "script too long for a short",
			validate: func() error {
				script := validScript()
				script.EstimatedDuration = domain.ShortsDuration(31 * time.Second)
				return script.Validate()
			},
			wantErr: domain.ErrDurationOutOfRange,
		},
		{
			name: "script not in russian",
			validate: func() error {
				script := validScript()
				script.Language = domain.LanguageEnglish
				return script.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "script.language",
		},
		{
			name: "media asset with zero size",
			validate: func() error {
				asset := validMediaAsset()
				asset.SizeBytes = 0
				return asset.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "media_asset.size_bytes",
		},
		{
			name: "media asset with unknown scheme",
			validate: func() error {
				asset := validMediaAsset()
				asset.URI = "ftp://media/vj-01/final.mp4"
				return asset.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "media_asset.uri",
		},
		{
			name: "published record without youtube id",
			validate: func() error {
				publication := validPublication()
				publication.YouTubeVideoID = ""
				return publication.Validate()
			},
			wantErr:   domain.ErrMissingField,
			wantField: "publication.youtube_video_id",
		},
		{
			name: "publication with malformed key",
			validate: func() error {
				publication := validPublication()
				publication.IdempotencyKey = "ключ с пробелом"
				return publication.Validate()
			},
			wantErr: domain.ErrInvalidValue,
		},
		{
			name: "cost with wrong total",
			validate: func() error {
				cost := validCost()
				cost.Total = domain.MicroUSD(999)
				return cost.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "cost.total",
		},
		{
			name: "cost with zero units",
			validate: func() error {
				cost := validCost()
				cost.Units = 0
				cost.Total = 0
				return cost.Validate()
			},
			wantErr:   domain.ErrInvalidValue,
			wantField: "cost.units",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.validate()
			if err == nil {
				t.Fatalf("Validate() error = nil, want %v", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantField == "" {
				return
			}
			var validationError *domain.ValidationError
			if !errors.As(err, &validationError) {
				t.Fatalf("error = %v, want a *domain.ValidationError", err)
			}
			if validationError.Field != tt.wantField {
				t.Fatalf("field = %q, want %q", validationError.Field, tt.wantField)
			}
		})
	}
}

func TestShortsDurationBounds(t *testing.T) {
	tests := []struct {
		name    string
		input   time.Duration
		wantErr error
	}{
		{"below minimum", 14999 * time.Millisecond, domain.ErrDurationOutOfRange},
		{"exactly minimum", 15 * time.Second, nil},
		{"typical", 22 * time.Second, nil},
		{"exactly maximum", 30 * time.Second, nil},
		{"above maximum", 30001 * time.Millisecond, domain.ErrDurationOutOfRange},
		{"zero", 0, domain.ErrDurationOutOfRange},
		{"negative", -5 * time.Second, domain.ErrDurationOutOfRange},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration, err := domain.NewShortsDuration(tt.input)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				if duration != 0 {
					t.Fatalf("duration = %v, want zero value", duration)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewShortsDuration() error = %v", err)
			}
			if duration.Duration() != tt.input {
				t.Fatalf("duration = %v, want %v", duration.Duration(), tt.input)
			}
		})
	}
}

func TestVideoValidationRules(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*domain.VideoValidation)
		wantErr    error
		wantAbsent bool
	}{
		{"valid", func(v *domain.VideoValidation) {}, nil, false},
		{"duration too short", func(v *domain.VideoValidation) {
			v.Duration = domain.ShortsDuration(5 * time.Second)
		}, domain.ErrDurationOutOfRange, false},
		{"no audio yet passed", func(v *domain.VideoValidation) {
			v.HasAudio = false
		}, domain.ErrInvalidValue, false},
		{"failed without reason", func(v *domain.VideoValidation) {
			v.Passed = false
		}, domain.ErrMissingField, false},
		{"zero width", func(v *domain.VideoValidation) {
			v.Width = 0
		}, domain.ErrInvalidValue, false},
		{"no check time", func(v *domain.VideoValidation) {
			v.CheckedAt = time.Time{}
		}, domain.ErrMissingField, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validation := passingValidation()
			tt.mutate(&validation)
			err := validation.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
