package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

var (
	testNow  = time.Date(2026, time.November, 12, 9, 0, 0, 0, time.UTC)
	baseTime = time.Date(2026, time.November, 12, 8, 0, 0, 0, time.UTC)
)

func validParams() domain.NewVideoJobParams {
	return domain.NewVideoJobParams{
		ID:             domain.VideoJobID("vj-2026-11-12-01"),
		DailyRunID:     domain.DailyRunID("run-2026-11-12"),
		StoryID:        domain.StoryID("story-01"),
		IdempotencyKey: domain.IdempotencyKey("run-2026-11-12/story-01/v1"),
		MaxAttempts:    3,
		CreatedAt:      testNow,
	}
}

func mustJob(t *testing.T) *domain.VideoJob {
	t.Helper()
	job, err := domain.NewVideoJob(validParams())
	if err != nil {
		t.Fatalf("NewVideoJob() error = %v", err)
	}
	return job
}

func passingValidation() domain.VideoValidation {
	return domain.VideoValidation{
		Duration:  domain.ShortsDuration(22 * time.Second),
		Width:     1080,
		Height:    1920,
		HasAudio:  true,
		Passed:    true,
		CheckedAt: testNow,
	}
}

func TestNewVideoJob(t *testing.T) {
	job, err := domain.NewVideoJob(validParams())
	if err != nil {
		t.Fatalf("NewVideoJob() error = %v", err)
	}
	if err := job.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if job.State() != domain.StateCreated {
		t.Fatalf("State() = %s, want %s", job.State(), domain.StateCreated)
	}
	if job.Attempts() != 0 {
		t.Fatalf("Attempts() = %d, want 0", job.Attempts())
	}
	if job.MaxAttempts() != 3 {
		t.Fatalf("MaxAttempts() = %d, want 3", job.MaxAttempts())
	}
	if job.PublicationID() != "" {
		t.Fatalf("PublicationID() = %q, want empty", job.PublicationID())
	}
	if _, ok := job.Validation(); ok {
		t.Fatal("Validation() reported a result for a fresh job")
	}
}

func TestNewVideoJobDefaultsMaxAttempts(t *testing.T) {
	params := validParams()
	params.MaxAttempts = 0
	job, err := domain.NewVideoJob(params)
	if err != nil {
		t.Fatalf("NewVideoJob() error = %v", err)
	}
	if job.MaxAttempts() != domain.DefaultMaxAttempts {
		t.Fatalf("MaxAttempts() = %d, want %d", job.MaxAttempts(), domain.DefaultMaxAttempts)
	}
}

func TestNewVideoJobRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.NewVideoJobParams)
		wantErr error
	}{
		{"missing id", func(p *domain.NewVideoJobParams) { p.ID = "" }, domain.ErrMissingField},
		{"missing run id", func(p *domain.NewVideoJobParams) { p.DailyRunID = "" }, domain.ErrMissingField},
		{"missing story id", func(p *domain.NewVideoJobParams) { p.StoryID = "" }, domain.ErrMissingField},
		{"missing idempotency key", func(p *domain.NewVideoJobParams) { p.IdempotencyKey = "" }, domain.ErrMissingField},
		{"malformed idempotency key", func(p *domain.NewVideoJobParams) { p.IdempotencyKey = "with space" }, domain.ErrInvalidValue},
		{"missing created at", func(p *domain.NewVideoJobParams) { p.CreatedAt = time.Time{} }, domain.ErrMissingField},
		{"negative attempts", func(p *domain.NewVideoJobParams) { p.MaxAttempts = -1 }, domain.ErrInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := validParams()
			tt.mutate(&params)
			job, err := domain.NewVideoJob(params)
			if err == nil {
				t.Fatalf("NewVideoJob() error = nil, want %v", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if job != nil {
				t.Fatalf("job = %v, want nil", job)
			}
		})
	}
}

var statePath = map[domain.VideoState][]domain.VideoState{
	domain.StateCreated:      nil,
	domain.StateScripted:     {domain.StateScripted},
	domain.StateMediaReady:   {domain.StateScripted, domain.StateMediaReady},
	domain.StateRendered:     {domain.StateScripted, domain.StateMediaReady, domain.StateRendered},
	domain.StateValidated:    {domain.StateScripted, domain.StateMediaReady, domain.StateRendered, domain.StateValidated},
	domain.StateUploaded:     {domain.StateScripted, domain.StateMediaReady, domain.StateRendered, domain.StateValidated, domain.StateUploaded},
	domain.StateScheduled:    {domain.StateScripted, domain.StateMediaReady, domain.StateRendered, domain.StateValidated, domain.StateUploaded, domain.StateScheduled},
	domain.StatePublished:    {domain.StateScripted, domain.StateMediaReady, domain.StateRendered, domain.StateValidated, domain.StateUploaded, domain.StateScheduled, domain.StatePublished},
	domain.StateFailed:       {domain.StateFailed},
	domain.StateRejected:     {domain.StateRejected},
	domain.StateRetryPending: {domain.StateScripted, domain.StateRetryPending},
}

// jobInState walks the happy path and returns a job in the requested state.
func jobInState(t *testing.T, target domain.VideoState) *domain.VideoJob {
	t.Helper()
	path, ok := statePath[target]
	if !ok {
		t.Fatalf("no known path to state %s", target)
	}
	job := mustJob(t)
	for _, next := range path {
		switch next {
		case domain.StateValidated:
			if err := job.SetValidation(passingValidation()); err != nil {
				t.Fatalf("SetValidation() error = %v", err)
			}
		case domain.StateScheduled:
			if err := job.AttachPublication(domain.PublicationID("pub-01"), testNow); err != nil {
				t.Fatalf("AttachPublication() error = %v", err)
			}
		}
		if err := job.Transition(next, testNow); err != nil {
			t.Fatalf("Transition(%s) error = %v", next, err)
		}
	}
	if job.State() != target {
		t.Fatalf("state = %s, want %s", job.State(), target)
	}
	return job
}

func withValidation(t *testing.T, job *domain.VideoJob) {
	t.Helper()
	if err := job.SetValidation(passingValidation()); err != nil {
		t.Fatalf("SetValidation() error = %v", err)
	}
}

func withPublication(t *testing.T, job *domain.VideoJob) {
	t.Helper()
	if err := job.AttachPublication(domain.PublicationID("pub-01"), testNow); err != nil {
		t.Fatalf("AttachPublication() error = %v", err)
	}
}

func TestTransitionTable(t *testing.T) {
	tests := []struct {
		name    string
		from    domain.VideoState
		to      domain.VideoState
		setup   func(*testing.T, *domain.VideoJob)
		wantErr error
	}{
		{"created to scripted", domain.StateCreated, domain.StateScripted, nil, nil},
		{"scripted to media ready", domain.StateScripted, domain.StateMediaReady, nil, nil},
		{"media ready to rendered", domain.StateMediaReady, domain.StateRendered, nil, nil},
		{"rendered to validated", domain.StateRendered, domain.StateValidated, withValidation, nil},
		{"validated to uploaded", domain.StateValidated, domain.StateUploaded, nil, nil},
		{"uploaded to scheduled", domain.StateUploaded, domain.StateScheduled, withPublication, nil},
		{"scheduled to published", domain.StateScheduled, domain.StatePublished, nil, nil},
		{"created to failed", domain.StateCreated, domain.StateFailed, nil, nil},
		{"scripted to rejected", domain.StateScripted, domain.StateRejected, nil, nil},
		{"failed to retry pending", domain.StateFailed, domain.StateRetryPending, nil, nil},
		{"retry pending resumes", domain.StateRetryPending, domain.StateScripted, nil, nil},
		{"retry pending skips ahead", domain.StateRetryPending, domain.StateMediaReady, nil, domain.ErrInvalidTransition},
		{"same state", domain.StateScripted, domain.StateScripted, nil, domain.ErrSameState},
		{"published to published", domain.StatePublished, domain.StatePublished, nil, domain.ErrSameState},
		{"skips script", domain.StateCreated, domain.StateMediaReady, nil, domain.ErrInvalidTransition},
		{"skips render", domain.StateCreated, domain.StateRendered, nil, domain.ErrInvalidTransition},
		{"skips everything", domain.StateCreated, domain.StatePublished, nil, domain.ErrInvalidTransition},
		{"skips media", domain.StateScripted, domain.StateValidated, nil, domain.ErrInvalidTransition},
		{"skips validation", domain.StateRendered, domain.StateUploaded, nil, domain.ErrInvalidTransition},
		{"skips upload", domain.StateValidated, domain.StateScheduled, nil, domain.ErrInvalidTransition},
		{"skips schedule", domain.StateUploaded, domain.StatePublished, nil, domain.ErrInvalidTransition},
		{"publishes without publication", domain.StateValidated, domain.StatePublished, nil, domain.ErrInvalidTransition},
		{"publishes after validation only", domain.StatePublished, domain.StateScheduled, nil, domain.ErrInvalidTransition},
		{"rejected is final", domain.StateRejected, domain.StateScripted, nil, domain.ErrInvalidTransition},
		{"unknown source", domain.VideoState("UNKNOWN"), domain.StateScripted, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.from == domain.VideoState("UNKNOWN") {
				// A job cannot be built in an unknown state through the public API,
				// so the static transition table is checked directly.
				err := domain.ValidateTransition(tt.from, tt.to)
				if !errors.Is(err, domain.ErrInvalidTransition) {
					t.Fatalf("error = %v, want a transition error", err)
				}
				return
			}
			job := jobInState(t, tt.from)
			if tt.setup != nil {
				tt.setup(t, job)
			}
			err := job.Transition(tt.to, testNow)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Transition(%s) error = %v", tt.to, err)
				}
				if job.State() != tt.to {
					t.Fatalf("state = %s, want %s", job.State(), tt.to)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Transition(%s) error = %v, want %v", tt.to, err, tt.wantErr)
			}
			if job.State() != tt.from {
				t.Fatalf("state = %s after rejected transition, want %s", job.State(), tt.from)
			}
		})
	}
}

func TestPublishWithoutValidationIsForbidden(t *testing.T) {
	job := jobInState(t, domain.StateRendered)
	if err := job.Transition(domain.StateUploaded, testNow); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("upload error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if err := job.Transition(domain.StatePublished, testNow); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("publish error = %v, want %v", err, domain.ErrInvalidTransition)
	}
	if err := job.Transition(domain.StateValidated, testNow); !errors.Is(err, domain.ErrValidationFailed) {
		t.Fatalf("validate error = %v, want %v", err, domain.ErrValidationFailed)
	}
	if job.State() != domain.StateRendered {
		t.Fatalf("state = %s, want %s", job.State(), domain.StateRendered)
	}
}

func TestFailedValidationBlocksUpload(t *testing.T) {
	job := jobInState(t, domain.StateRendered)
	validation := passingValidation()
	validation.Passed = false
	validation.Reason = "no audio track"
	if err := job.SetValidation(validation); err != nil {
		t.Fatalf("SetValidation() error = %v", err)
	}
	if err := job.Transition(domain.StateValidated, testNow); !errors.Is(err, domain.ErrValidationFailed) {
		t.Fatalf("error = %v, want %v", err, domain.ErrValidationFailed)
	}
	if err := job.Transition(domain.StateUploaded, testNow); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestValidationIsRecordedOnlyAfterRender(t *testing.T) {
	job := mustJob(t)
	if err := job.SetValidation(passingValidation()); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestSinglePublicationPerJob(t *testing.T) {
	job := jobInState(t, domain.StateUploaded)
	if err := job.AttachPublication(domain.PublicationID("pub-01"), testNow); err != nil {
		t.Fatalf("AttachPublication() error = %v", err)
	}
	if err := job.AttachPublication(domain.PublicationID("pub-02"), testNow); !errors.Is(err, domain.ErrAlreadyPublished) {
		t.Fatalf("second AttachPublication() error = %v, want %v", err, domain.ErrAlreadyPublished)
	}
	if job.PublicationID() != domain.PublicationID("pub-01") {
		t.Fatalf("PublicationID() = %s, want pub-01", job.PublicationID())
	}
	if err := job.Transition(domain.StateScheduled, testNow); err != nil {
		t.Fatalf("Transition(SCHEDULED) error = %v", err)
	}
	if err := job.Transition(domain.StatePublished, testNow); err != nil {
		t.Fatalf("Transition(PUBLISHED) error = %v", err)
	}
	if job.PublishedAt().IsZero() {
		t.Fatal("PublishedAt() is zero after publishing")
	}
	if err := job.Transition(domain.StatePublished, testNow); !errors.Is(err, domain.ErrSameState) {
		t.Fatalf("repeated publish error = %v, want %v", err, domain.ErrSameState)
	}
	if err := job.Transition(domain.StateScheduled, testNow); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("revert error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestPublicationRequiresUploadedState(t *testing.T) {
	job := jobInState(t, domain.StateValidated)
	if err := job.AttachPublication(domain.PublicationID("pub-01"), testNow); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("error = %v, want %v", err, domain.ErrInvalidTransition)
	}
}

func TestRetryAttemptsAreLimited(t *testing.T) {
	job := mustJob(t)
	for attempt := 1; attempt <= job.MaxAttempts(); attempt++ {
		if err := job.Transition(domain.StateRetryPending, testNow); err != nil {
			t.Fatalf("attempt %d: Transition(RETRY_PENDING) error = %v", attempt, err)
		}
		if job.Attempts() != attempt {
			t.Fatalf("attempts = %d, want %d", job.Attempts(), attempt)
		}
		if err := job.Transition(domain.StateCreated, testNow); err != nil {
			t.Fatalf("attempt %d: resume error = %v", attempt, err)
		}
		if job.ResumeState() != "" {
			t.Fatalf("resume state = %s, want empty after resume", job.ResumeState())
		}
	}
	if err := job.Transition(domain.StateRetryPending, testNow); !errors.Is(err, domain.ErrRetriesExhausted) {
		t.Fatalf("error = %v, want %v", err, domain.ErrRetriesExhausted)
	}
}

func TestRetryFromFailureKeepsResumeState(t *testing.T) {
	job := jobInState(t, domain.StateScripted)
	if err := job.Transition(domain.StateFailed, testNow); err != nil {
		t.Fatalf("Transition(FAILED) error = %v", err)
	}
	if job.ResumeState() != domain.StateScripted {
		t.Fatalf("ResumeState() = %s, want %s", job.ResumeState(), domain.StateScripted)
	}
	if err := job.Transition(domain.StateRetryPending, testNow); err != nil {
		t.Fatalf("Transition(RETRY_PENDING) error = %v", err)
	}
	if job.Attempts() != 1 {
		t.Fatalf("Attempts() = %d, want 1", job.Attempts())
	}
	if err := job.Transition(domain.StateScripted, testNow); err != nil {
		t.Fatalf("resume error = %v", err)
	}
	if job.State() != domain.StateScripted {
		t.Fatalf("state = %s, want %s", job.State(), domain.StateScripted)
	}
}

func TestTransitionRequiresTimestamp(t *testing.T) {
	job := mustJob(t)
	if err := job.Transition(domain.StateScripted, time.Time{}); !errors.Is(err, domain.ErrMissingField) {
		t.Fatalf("error = %v, want %v", err, domain.ErrMissingField)
	}
}
