package workflows

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/audio"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/notify"
	"github.com/it-news-shorts/it-news-shorts/internal/script"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
	"github.com/it-news-shorts/it-news-shorts/internal/video"
	"github.com/it-news-shorts/it-news-shorts/internal/youtube"
)

// ErrMissingStep is returned when a workflow dependency is not wired.
var ErrMissingStep = errors.New("workflows: missing step")

// DailyRunInput is the deterministic input of one run. Temporal will replay it
// from history, so it contains no wall clock reads and no randomness.
type DailyRunInput struct {
	RunID      domain.DailyRunID
	RunAt      time.Time
	Window     domain.TimeWindow
	MaxStories int
}

// Validate enforces the input rules.
func (i DailyRunInput) Validate() error {
	if i.RunID == "" {
		return domain.MissingField("daily_run_input.run_id")
	}
	if i.RunAt.IsZero() {
		return domain.MissingField("daily_run_input.run_at")
	}
	if err := i.Window.Validate(); err != nil {
		return err
	}
	if i.MaxStories < 1 || i.MaxStories > domain.MaxStoriesPerRun {
		return domain.InvalidValue("daily_run_input.max_stories")
	}
	return nil
}

// DailyRunResult summarizes a run.
type DailyRunResult struct {
	RunID   domain.DailyRunID
	Stories []domain.StoryID
	Jobs    []domain.VideoJobID
	Costs   domain.MicroUSD
}

// DailyRunWorkflow is the orchestration contract implemented with Temporal in
// a later stage.
type DailyRunWorkflow interface {
	Run(ctx context.Context, in DailyRunInput) (DailyRunResult, error)
}

// Steps holds the ports used by the workflow.
type Steps struct {
	News        news.NewsCollector
	Ranker      ai.StoryRanker
	FactChecker ai.FactChecker
	Scripts     script.ScriptGenerator
	TTS         audio.TTSProvider
	VideoGen    video.VideoGenerationProvider
	Renderer    video.VideoRenderer
	Validator   video.VideoValidator
	Publisher   youtube.VideoPublisher
	Storage     storage.ObjectStorage
	Notifier    notify.Notifier
}

// Validate reports every unwired dependency.
func (s Steps) Validate() error {
	var absent []string
	if s.News == nil {
		absent = append(absent, "news collector")
	}
	if s.Ranker == nil {
		absent = append(absent, "story ranker")
	}
	if s.FactChecker == nil {
		absent = append(absent, "fact checker")
	}
	if s.Scripts == nil {
		absent = append(absent, "script generator")
	}
	if s.TTS == nil {
		absent = append(absent, "tts provider")
	}
	if s.VideoGen == nil {
		absent = append(absent, "video generation provider")
	}
	if s.Renderer == nil {
		absent = append(absent, "video renderer")
	}
	if s.Validator == nil {
		absent = append(absent, "video validator")
	}
	if s.Publisher == nil {
		absent = append(absent, "video publisher")
	}
	if s.Storage == nil {
		absent = append(absent, "object storage")
	}
	if s.Notifier == nil {
		absent = append(absent, "notifier")
	}
	if len(absent) > 0 {
		return errors.Join(ErrMissingStep, errors.New(strings.Join(absent, ", ")))
	}
	return nil
}
