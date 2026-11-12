package domain

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Core domain constants.
const (
	// NewsWindow is the lookback period of every daily run.
	NewsWindow = 24 * time.Hour
	// MaxStoriesPerRun is the maximum number of shorts produced per day.
	MaxStoriesPerRun = 3
	// DailyRunHourMSK is the publishing schedule hour in Europe/Moscow.
	DailyRunHourMSK = 9
)

// TimeWindow is a half-open interval [Start, End) used to select news.
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// NewTimeWindow validates the window.
func NewTimeWindow(start, end time.Time) (TimeWindow, error) {
	window := TimeWindow{Start: start, End: end}
	if err := window.Validate(); err != nil {
		return TimeWindow{}, err
	}
	return window, nil
}

// Validate requires a exactly 24 hour long window.
func (w TimeWindow) Validate() error {
	if w.Start.IsZero() {
		return missing("window.start")
	}
	if w.End.IsZero() {
		return missing("window.end")
	}
	if !w.End.After(w.Start) {
		return invalid("window.end")
	}
	if w.End.Sub(w.Start) != NewsWindow {
		return &ValidationError{Field: "window.duration", Err: ErrInvalidWindow}
	}
	return nil
}

// Duration returns the window length.
func (w TimeWindow) Duration() time.Duration { return w.End.Sub(w.Start) }

// Contains reports whether t belongs to [Start, End).
func (w TimeWindow) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// Article is a single news item collected from a source.
type Article struct {
	ID          ArticleID
	URL         string
	Title       string
	Summary     string
	Source      string
	Language    Language
	PublishedAt time.Time
	FetchedAt   time.Time
}

// Validate enforces the article rules.
func (a Article) Validate() error {
	if a.ID == "" {
		return missing("article.id")
	}
	if err := validateURL("article.url", a.URL); err != nil {
		return err
	}
	if strings.TrimSpace(a.Title) == "" {
		return missing("article.title")
	}
	if strings.TrimSpace(a.Source) == "" {
		return missing("article.source")
	}
	if err := a.Language.Validate(); err != nil {
		return err
	}
	if a.PublishedAt.IsZero() {
		return missing("article.published_at")
	}
	if a.FetchedAt.IsZero() {
		return missing("article.fetched_at")
	}
	if a.FetchedAt.Before(a.PublishedAt) {
		return invalid("article.fetched_at")
	}
	return nil
}

// StoryEvidence ties a story claim to the article that supports it.
type StoryEvidence struct {
	ArticleID   ArticleID
	Source      string
	URL         string
	Quote       string
	PublishedAt time.Time
}

// Validate enforces the evidence rules.
func (e StoryEvidence) Validate() error {
	if e.ArticleID == "" {
		return missing("evidence.article_id")
	}
	if strings.TrimSpace(e.Source) == "" {
		return missing("evidence.source")
	}
	if err := validateURL("evidence.url", e.URL); err != nil {
		return err
	}
	if strings.TrimSpace(e.Quote) == "" {
		return missing("evidence.quote")
	}
	if e.PublishedAt.IsZero() {
		return missing("evidence.published_at")
	}
	return nil
}

// Story is a news candidate selected for a short.
type Story struct {
	ID         StoryID
	DailyRunID DailyRunID
	Title      string
	Summary    string
	Score      float64
	// Position is the rank inside a daily run: 1..MaxStoriesPerRun once the
	// story is selected. Zero means the candidate has not been selected yet,
	// which is the normal state while candidates are ranked.
	Position  int
	Evidence  []StoryEvidence
	CreatedAt time.Time
}

// Validate enforces the story rules. A story without evidence is invalid:
// nothing is published without a verifiable source.
func (s Story) Validate() error {
	if s.ID == "" {
		return missing("story.id")
	}
	if s.DailyRunID == "" {
		return missing("story.daily_run_id")
	}
	if strings.TrimSpace(s.Title) == "" {
		return missing("story.title")
	}
	if s.Score < 0 || s.Score > 1 {
		return invalid("story.score")
	}
	if s.Position < 0 || s.Position > MaxStoriesPerRun {
		return invalid("story.position")
	}
	if len(s.Evidence) == 0 {
		return missing("story.evidence")
	}
	for i, evidence := range s.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("story.evidence[%d]: %w", i, err)
		}
	}
	if s.CreatedAt.IsZero() {
		return missing("story.created_at")
	}
	return nil
}

// DailyRunState is the state of one daily production run.
type DailyRunState string

// Daily run states.
const (
	DailyRunPending   DailyRunState = "PENDING"
	DailyRunRunning   DailyRunState = "RUNNING"
	DailyRunCompleted DailyRunState = "COMPLETED"
	DailyRunFailed    DailyRunState = "FAILED"
)

// Valid reports whether the state is known.
func (s DailyRunState) Valid() bool {
	switch s {
	case DailyRunPending, DailyRunRunning, DailyRunCompleted, DailyRunFailed:
		return true
	default:
		return false
	}
}

// Terminal reports whether the run is finished.
func (s DailyRunState) Terminal() bool {
	return s == DailyRunCompleted || s == DailyRunFailed
}

// DailyRun is one scheduled execution of the pipeline.
type DailyRun struct {
	ID         DailyRunID
	RunDate    time.Time
	Window     TimeWindow
	State      DailyRunState
	MaxStories int
	CreatedAt  time.Time
}

// Validate enforces the daily run rules.
func (r DailyRun) Validate() error {
	if r.ID == "" {
		return missing("daily_run.id")
	}
	if r.RunDate.IsZero() {
		return missing("daily_run.run_date")
	}
	if err := r.Window.Validate(); err != nil {
		return err
	}
	if !r.State.Valid() {
		return invalid("daily_run.state")
	}
	if r.MaxStories < 1 || r.MaxStories > MaxStoriesPerRun {
		return invalid("daily_run.max_stories")
	}
	if r.CreatedAt.IsZero() {
		return missing("daily_run.created_at")
	}
	return nil
}

// Script is the narration text of one short.
type Script struct {
	ID                ScriptID
	VideoJobID        VideoJobID
	Language          Language
	Narration         string
	EstimatedDuration ShortsDuration
	Version           int
	CreatedAt         time.Time
}

// Validate enforces the script rules: Russian narration that fits a short.
func (s Script) Validate() error {
	if s.ID == "" {
		return missing("script.id")
	}
	if s.VideoJobID == "" {
		return missing("script.video_job_id")
	}
	if s.Language != LanguageRussian {
		return invalid("script.language")
	}
	if strings.TrimSpace(s.Narration) == "" {
		return missing("script.narration")
	}
	if err := s.EstimatedDuration.Validate(); err != nil {
		return err
	}
	if s.Version < 1 {
		return invalid("script.version")
	}
	if s.CreatedAt.IsZero() {
		return missing("script.created_at")
	}
	return nil
}

// MediaKind classifies stored media assets.
type MediaKind string

// Media kinds.
const (
	MediaVoiceover  MediaKind = "VOICEOVER"
	MediaBackground MediaKind = "BACKGROUND"
	MediaClip       MediaKind = "CLIP"
	MediaRendered   MediaKind = "RENDERED_VIDEO"
	MediaThumbnail  MediaKind = "THUMBNAIL"
)

// Valid reports whether the kind is known.
func (k MediaKind) Valid() bool {
	switch k {
	case MediaVoiceover, MediaBackground, MediaClip, MediaRendered, MediaThumbnail:
		return true
	default:
		return false
	}
}

// MediaAsset is any produced or downloaded binary artifact.
type MediaAsset struct {
	ID         MediaAssetID
	VideoJobID VideoJobID
	Kind       MediaKind
	URI        string
	MIMEType   string
	SizeBytes  int64
	Duration   time.Duration
	Checksum   string
	Provider   string
	CreatedAt  time.Time
}

// Validate enforces the media asset rules.
func (a MediaAsset) Validate() error {
	if a.ID == "" {
		return missing("media_asset.id")
	}
	if a.VideoJobID == "" {
		return missing("media_asset.video_job_id")
	}
	if !a.Kind.Valid() {
		return invalid("media_asset.kind")
	}
	if err := validateObjectURI("media_asset.uri", a.URI); err != nil {
		return err
	}
	if strings.TrimSpace(a.MIMEType) == "" {
		return missing("media_asset.mime_type")
	}
	if a.SizeBytes <= 0 {
		return invalid("media_asset.size_bytes")
	}
	if a.Duration < 0 {
		return invalid("media_asset.duration")
	}
	if strings.TrimSpace(a.Checksum) == "" {
		return missing("media_asset.checksum")
	}
	if strings.TrimSpace(a.Provider) == "" {
		return missing("media_asset.provider")
	}
	if a.CreatedAt.IsZero() {
		return missing("media_asset.created_at")
	}
	return nil
}

// PublicationStatus is the state of a YouTube publication.
type PublicationStatus string

// Publication states.
const (
	PublicationPending   PublicationStatus = "PENDING"
	PublicationScheduled PublicationStatus = "SCHEDULED"
	PublicationPublished PublicationStatus = "PUBLISHED"
	PublicationFailed    PublicationStatus = "FAILED"
)

// Valid reports whether the status is known.
func (p PublicationStatus) Valid() bool {
	switch p {
	case PublicationPending, PublicationScheduled, PublicationPublished, PublicationFailed:
		return true
	default:
		return false
	}
}

// Publication is the external YouTube record of one video job.
type Publication struct {
	ID             PublicationID
	VideoJobID     VideoJobID
	YouTubeVideoID string
	Status         PublicationStatus
	IdempotencyKey IdempotencyKey
	ScheduledAt    time.Time
	PublishedAt    time.Time
	CreatedAt      time.Time
}

// Validate enforces the publication rules.
func (p Publication) Validate() error {
	if p.ID == "" {
		return missing("publication.id")
	}
	if p.VideoJobID == "" {
		return missing("publication.video_job_id")
	}
	if err := p.IdempotencyKey.Validate(); err != nil {
		return err
	}
	if !p.Status.Valid() {
		return invalid("publication.status")
	}
	if p.CreatedAt.IsZero() {
		return missing("publication.created_at")
	}
	switch p.Status {
	case PublicationPublished:
		if strings.TrimSpace(p.YouTubeVideoID) == "" {
			return missing("publication.youtube_video_id")
		}
		if p.PublishedAt.IsZero() {
			return missing("publication.published_at")
		}
	case PublicationScheduled:
		if p.ScheduledAt.IsZero() {
			return missing("publication.scheduled_at")
		}
	}
	return nil
}

// GenerationCost is one billable unit of an external provider.
type GenerationCost struct {
	ID         CostID
	VideoJobID VideoJobID
	Provider   string
	Operation  string
	Units      int64
	UnitCost   MicroUSD
	Total      MicroUSD
	CreatedAt  time.Time
}

// Validate enforces the cost rules, including the arithmetic invariant.
func (c GenerationCost) Validate() error {
	if c.ID == "" {
		return missing("cost.id")
	}
	if c.VideoJobID == "" {
		return missing("cost.video_job_id")
	}
	if strings.TrimSpace(c.Provider) == "" {
		return missing("cost.provider")
	}
	if strings.TrimSpace(c.Operation) == "" {
		return missing("cost.operation")
	}
	if c.Units < 1 {
		return invalid("cost.units")
	}
	if err := c.UnitCost.Validate(); err != nil {
		return err
	}
	if err := c.Total.Validate(); err != nil {
		return err
	}
	expected := MicroUSD(c.Units * int64(c.UnitCost))
	if c.Total != expected {
		return invalid("cost.total")
	}
	if c.CreatedAt.IsZero() {
		return missing("cost.created_at")
	}
	return nil
}

func validateURL(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return missing(field)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid(field)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return invalid(field)
	}
	if parsed.Host == "" {
		return invalid(field)
	}
	return nil
}

func validateObjectURI(field, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return missing(field)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return invalid(field)
	}
	switch parsed.Scheme {
	case "http", "https", "s3", "minio", "file":
		return nil
	default:
		return invalid(field)
	}
}
