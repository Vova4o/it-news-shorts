package domain

import (
	"fmt"
	"time"
)

// DefaultMaxAttempts is used when a job is created without an explicit limit.
const DefaultMaxAttempts = 3

// VideoJob is the aggregate root of the production pipeline. All state changes
// go through Transition, which enforces the state machine and its preconditions.
type VideoJob struct {
	id             VideoJobID
	dailyRunID     DailyRunID
	storyID        StoryID
	idempotencyKey IdempotencyKey
	state          VideoState
	resumeState    VideoState
	attempts       int
	maxAttempts    int
	validation     *VideoValidation
	publicationID  PublicationID
	createdAt      time.Time
	updatedAt      time.Time
	validatedAt    time.Time
	uploadedAt     time.Time
	scheduledAt    time.Time
	publishedAt    time.Time
}

// NewVideoJobParams describes a new job.
type NewVideoJobParams struct {
	ID             VideoJobID
	DailyRunID     DailyRunID
	StoryID        StoryID
	IdempotencyKey IdempotencyKey
	MaxAttempts    int
	CreatedAt      time.Time
}

// NewVideoJob creates a job in the CREATED state.
func NewVideoJob(p NewVideoJobParams) (*VideoJob, error) {
	job := &VideoJob{
		id:             p.ID,
		dailyRunID:     p.DailyRunID,
		storyID:        p.StoryID,
		idempotencyKey: p.IdempotencyKey,
		state:          StateCreated,
		maxAttempts:    p.MaxAttempts,
		createdAt:      p.CreatedAt,
	}
	if job.maxAttempts == 0 {
		job.maxAttempts = DefaultMaxAttempts
	}
	if job.maxAttempts < 0 {
		return nil, invalid("video_job.max_attempts")
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	job.updatedAt = job.createdAt
	return job, nil
}

// Validate enforces the aggregate invariants.
func (j *VideoJob) Validate() error {
	if j.id == "" {
		return missing("video_job.id")
	}
	if j.dailyRunID == "" {
		return missing("video_job.daily_run_id")
	}
	if j.storyID == "" {
		return missing("video_job.story_id")
	}
	if err := j.idempotencyKey.Validate(); err != nil {
		return err
	}
	if j.maxAttempts < 1 {
		return invalid("video_job.max_attempts")
	}
	if j.attempts < 0 || j.attempts > j.maxAttempts {
		return invalid("video_job.attempts")
	}
	if !j.state.Valid() {
		return invalid("video_job.state")
	}
	if j.createdAt.IsZero() {
		return missing("video_job.created_at")
	}
	if j.state == StateRetryPending && !j.resumeState.Valid() {
		return invalid("video_job.resume_state")
	}
	return nil
}

// ID returns the job identifier.
func (j *VideoJob) ID() VideoJobID { return j.id }

// DailyRunID returns the owning run.
func (j *VideoJob) DailyRunID() DailyRunID { return j.dailyRunID }

// StoryID returns the story the short is based on.
func (j *VideoJob) StoryID() StoryID { return j.storyID }

// IdempotencyKey returns the key of the job.
func (j *VideoJob) IdempotencyKey() IdempotencyKey { return j.idempotencyKey }

// State returns the current lifecycle state.
func (j *VideoJob) State() VideoState { return j.state }

// ResumeState returns the state a RETRY_PENDING job returns to.
func (j *VideoJob) ResumeState() VideoState { return j.resumeState }

// Attempts returns the number of retries already started.
func (j *VideoJob) Attempts() int { return j.attempts }

// MaxAttempts returns the retry limit.
func (j *VideoJob) MaxAttempts() int { return j.maxAttempts }

// PublicationID returns the attached publication, if any.
func (j *VideoJob) PublicationID() PublicationID { return j.publicationID }

// Validation returns the recorded validation result, if any.
func (j *VideoJob) Validation() (VideoValidation, bool) {
	if j.validation == nil {
		return VideoValidation{}, false
	}
	return *j.validation, true
}

// CreatedAt returns the creation moment.
func (j *VideoJob) CreatedAt() time.Time { return j.createdAt }

// UpdatedAt returns the last successful transition moment.
func (j *VideoJob) UpdatedAt() time.Time { return j.updatedAt }

// ValidatedAt returns the moment the job entered VALIDATED.
func (j *VideoJob) ValidatedAt() time.Time { return j.validatedAt }

// UploadedAt returns the moment the job entered UPLOADED.
func (j *VideoJob) UploadedAt() time.Time { return j.uploadedAt }

// ScheduledAt returns the moment the job entered SCHEDULED.
func (j *VideoJob) ScheduledAt() time.Time { return j.scheduledAt }

// PublishedAt returns the moment the job entered PUBLISHED.
func (j *VideoJob) PublishedAt() time.Time { return j.publishedAt }

// SetValidation records the render validation. It is only possible in the
// RENDERED state, so a job cannot be marked valid before it exists.
func (j *VideoJob) SetValidation(validation VideoValidation) error {
	if j.state != StateRendered {
		return fmt.Errorf("%w: validation can be recorded only in %s, job %s is %s",
			ErrInvalidTransition, StateRendered, j.id, j.state)
	}
	if err := validation.Validate(); err != nil {
		return err
	}
	j.validation = &validation
	return nil
}

// AttachPublication binds the YouTube publication to the job. A job can own
// exactly one publication, which makes double publishing impossible.
func (j *VideoJob) AttachPublication(id PublicationID, at time.Time) error {
	if id == "" {
		return missing("publication.id")
	}
	if j.publicationID != "" {
		return fmt.Errorf("%w: video job %s already has publication %s", ErrAlreadyPublished, j.id, j.publicationID)
	}
	if j.state != StateUploaded && j.state != StateScheduled {
		return fmt.Errorf("%w: publication can be attached only after %s, job %s is %s",
			ErrInvalidTransition, StateUploaded, j.id, j.state)
	}
	if j.validation == nil || !j.validation.Passed {
		return fmt.Errorf("%w: video job %s has no passed validation", ErrValidationRequired, j.id)
	}
	j.publicationID = id
	j.updatedAt = at
	return nil
}

// Transition moves the job to the target state or returns a domain error.
func (j *VideoJob) Transition(to VideoState, at time.Time) error {
	if at.IsZero() {
		return missing("video_job.transition_at")
	}
	if err := j.Validate(); err != nil {
		return err
	}
	if j.state == to {
		return fmt.Errorf("%w: video job %s is already in state %s", ErrSameState, j.id, to)
	}
	if to == StateRetryPending {
		return j.beginRetry(at)
	}

	from := j.state
	if from == StateRetryPending {
		if !j.resumeState.Valid() {
			return fmt.Errorf("%w: video job %s has no resume state", ErrInvalidTransition, j.id)
		}
		if to != j.resumeState {
			return fmt.Errorf("%w: video job %s must resume to %s, got %s",
				ErrInvalidTransition, j.id, j.resumeState, to)
		}
	} else if err := ValidateTransition(from, to); err != nil {
		return err
	}
	if err := j.guard(to); err != nil {
		return err
	}
	if to == StateFailed && j.resumeState == "" && retryable(from) {
		j.resumeState = from
	}
	j.apply(to, at)
	if from == StateRetryPending {
		j.resumeState = ""
	}
	return nil
}

func (j *VideoJob) beginRetry(at time.Time) error {
	if j.attempts >= j.maxAttempts {
		return fmt.Errorf("%w: video job %s used %d of %d attempts",
			ErrRetriesExhausted, j.id, j.attempts, j.maxAttempts)
	}
	resume := j.state
	if resume == StateFailed {
		resume = j.resumeState
	}
	if !retryable(resume) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, j.state, StateRetryPending)
	}
	if j.state != StateFailed {
		if err := ValidateTransition(j.state, StateRetryPending); err != nil {
			return err
		}
	}
	j.resumeState = resume
	j.attempts++
	j.state = StateRetryPending
	j.updatedAt = at
	return nil
}

func (j *VideoJob) guard(to VideoState) error {
	switch to {
	case StateValidated:
		if j.validation == nil {
			return fmt.Errorf("%w: video job %s has no validation result", ErrValidationFailed, j.id)
		}
		if !j.validation.Passed {
			return fmt.Errorf("%w: video job %s failed validation: %s",
				ErrValidationFailed, j.id, j.validation.Reason)
		}
	case StateUploaded:
		if j.validation == nil || !j.validation.Passed {
			return fmt.Errorf("%w: video job %s cannot be uploaded before validation",
				ErrValidationRequired, j.id)
		}
	case StateScheduled, StatePublished:
		if j.publicationID == "" {
			return fmt.Errorf("%w: video job %s has no publication", ErrPublicationNotAllowed, j.id)
		}
		if j.validation == nil || !j.validation.Passed {
			return fmt.Errorf("%w: video job %s cannot be published before validation",
				ErrValidationRequired, j.id)
		}
	}
	return nil
}

func (j *VideoJob) apply(to VideoState, at time.Time) {
	j.state = to
	j.updatedAt = at
	switch to {
	case StateValidated:
		j.validatedAt = at
	case StateUploaded:
		j.uploadedAt = at
	case StateScheduled:
		j.scheduledAt = at
	case StatePublished:
		j.publishedAt = at
	}
}

// VideoValidation is the technical check of a rendered file.
type VideoValidation struct {
	Duration  ShortsDuration
	Width     int
	Height    int
	HasAudio  bool
	Passed    bool
	Reason    string
	CheckedAt time.Time
}

// Validate enforces the validation record rules.
func (v VideoValidation) Validate() error {
	if err := v.Duration.Validate(); err != nil {
		return err
	}
	if v.Width <= 0 {
		return invalid("validation.width")
	}
	if v.Height <= 0 {
		return invalid("validation.height")
	}
	if v.HasAudio && !v.Passed && v.Reason == "" {
		return missing("validation.reason")
	}
	if !v.HasAudio && v.Passed {
		return invalid("validation.has_audio")
	}
	if !v.Passed && v.Reason == "" {
		return missing("validation.reason")
	}
	if v.CheckedAt.IsZero() {
		return missing("validation.checked_at")
	}
	return nil
}

// VideoJobSnapshot is the persistence-friendly representation of VideoJob.
type VideoJobSnapshot struct {
	ID             VideoJobID
	DailyRunID     DailyRunID
	StoryID        StoryID
	IdempotencyKey IdempotencyKey
	State          VideoState
	ResumeState    VideoState
	Attempts       int
	MaxAttempts    int
	Validation     *VideoValidation
	PublicationID  PublicationID
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ValidatedAt    time.Time
	UploadedAt     time.Time
	ScheduledAt    time.Time
	PublishedAt    time.Time
}

// Snapshot returns a copy of the job state for persistence.
func (j *VideoJob) Snapshot() VideoJobSnapshot {
	var valCopy *VideoValidation
	if j.validation != nil {
		v := *j.validation
		valCopy = &v
	}
	return VideoJobSnapshot{
		ID:             j.id,
		DailyRunID:     j.dailyRunID,
		StoryID:        j.storyID,
		IdempotencyKey: j.idempotencyKey,
		State:          j.state,
		ResumeState:    j.resumeState,
		Attempts:       j.attempts,
		MaxAttempts:    j.maxAttempts,
		Validation:     valCopy,
		PublicationID:  j.publicationID,
		CreatedAt:      j.createdAt,
		UpdatedAt:      j.updatedAt,
		ValidatedAt:    j.validatedAt,
		UploadedAt:     j.uploadedAt,
		ScheduledAt:    j.scheduledAt,
		PublishedAt:    j.publishedAt,
	}
}

// RehydrateVideoJob restores a VideoJob from persisted storage while enforcing
// domain invariants.
func RehydrateVideoJob(s VideoJobSnapshot) (*VideoJob, error) {
	var valCopy *VideoValidation
	if s.Validation != nil {
		if err := s.Validation.Validate(); err != nil {
			return nil, err
		}
		v := *s.Validation
		valCopy = &v
	}
	job := &VideoJob{
		id:             s.ID,
		dailyRunID:     s.DailyRunID,
		storyID:        s.StoryID,
		idempotencyKey: s.IdempotencyKey,
		state:          s.State,
		resumeState:    s.ResumeState,
		attempts:       s.Attempts,
		maxAttempts:    s.MaxAttempts,
		validation:     valCopy,
		publicationID:  s.PublicationID,
		createdAt:      s.CreatedAt.UTC(),
		updatedAt:      s.UpdatedAt.UTC(),
		validatedAt:    toUTC(s.ValidatedAt),
		uploadedAt:     toUTC(s.UploadedAt),
		scheduledAt:    toUTC(s.ScheduledAt),
		publishedAt:    toUTC(s.PublishedAt),
	}
	if job.updatedAt.IsZero() {
		job.updatedAt = job.createdAt
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	if err := job.validateRehydratedLifecycle(); err != nil {
		return nil, err
	}
	return job, nil
}

func (j *VideoJob) validateRehydratedLifecycle() error {
	if j.updatedAt.Before(j.createdAt) {
		return invalid("video_job.updated_at")
	}

	// Check state-specific guard preconditions (passed validation, publication).
	switch j.state {
	case StateValidated, StateUploaded, StateScheduled, StatePublished:
		if err := j.guard(j.state); err != nil {
			return err
		}
	case StateRetryPending:
		if j.attempts < 1 {
			return invalid("video_job.attempts")
		}
		if !retryable(j.resumeState) {
			return invalid("video_job.resume_state")
		}
		if err := j.guard(j.resumeState); err != nil {
			return err
		}
	case StateFailed, StateRejected:
		if j.resumeState != "" && (!j.resumeState.Valid() || !retryable(j.resumeState)) {
			return invalid("video_job.resume_state")
		}
	default:
		if j.resumeState != "" {
			return invalid("video_job.resume_state")
		}
	}

	// If a publication is attached, AttachPublication preconditions must hold.
	if j.publicationID != "" {
		if j.validation == nil || !j.validation.Passed {
			return fmt.Errorf("%w: video job %s has publication without passed validation", ErrValidationRequired, j.id)
		}
		if j.uploadedAt.IsZero() {
			return missing("video_job.uploaded_at")
		}
	}

	// Enforce required lifecycle timestamps for reached states.
	effectiveState := j.state
	if (j.state == StateRetryPending || j.state == StateFailed) && j.resumeState != "" {
		effectiveState = j.resumeState
	}
	switch effectiveState {
	case StateValidated:
		if j.validatedAt.IsZero() {
			return missing("video_job.validated_at")
		}
	case StateUploaded:
		if j.validatedAt.IsZero() {
			return missing("video_job.validated_at")
		}
		if j.uploadedAt.IsZero() {
			return missing("video_job.uploaded_at")
		}
	case StateScheduled:
		if j.validatedAt.IsZero() {
			return missing("video_job.validated_at")
		}
		if j.uploadedAt.IsZero() {
			return missing("video_job.uploaded_at")
		}
		if j.scheduledAt.IsZero() {
			return missing("video_job.scheduled_at")
		}
	case StatePublished:
		if j.validatedAt.IsZero() {
			return missing("video_job.validated_at")
		}
		if j.uploadedAt.IsZero() {
			return missing("video_job.uploaded_at")
		}
		if j.scheduledAt.IsZero() {
			return missing("video_job.scheduled_at")
		}
		if j.publishedAt.IsZero() {
			return missing("video_job.published_at")
		}
	}

	// Enforce monotonic timestamp ordering for any populated lifecycle timestamps.
	if err := checkTimestampOrder(j.createdAt, j.updatedAt, j.validatedAt, j.uploadedAt, j.scheduledAt, j.publishedAt); err != nil {
		return err
	}
	if !j.uploadedAt.IsZero() && j.validatedAt.IsZero() {
		return missing("video_job.validated_at")
	}
	if !j.scheduledAt.IsZero() && (j.uploadedAt.IsZero() || j.publicationID == "") {
		return invalid("video_job.scheduled_at")
	}
	if !j.publishedAt.IsZero() && (j.scheduledAt.IsZero() || j.publicationID == "") {
		return invalid("video_job.published_at")
	}
	return nil
}

func checkTimestampOrder(createdAt, updatedAt time.Time, stamps ...time.Time) error {
	prev := createdAt
	for _, ts := range stamps {
		if ts.IsZero() {
			continue
		}
		if ts.Before(createdAt) || ts.After(updatedAt) || ts.Before(prev) {
			return invalid("video_job.lifecycle_timestamps")
		}
		prev = ts
	}
	return nil
}

func toUTC(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC()
}
