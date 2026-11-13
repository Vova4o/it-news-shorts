package domain_test

import (
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func TestRehydrateVideoJobRejectsBypassedInvariants(t *testing.T) {
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	validPass := &domain.VideoValidation{
		Duration:  domain.ShortsDuration(30 * time.Second),
		Width:     1080,
		Height:    1920,
		HasAudio:  true,
		Passed:    true,
		CheckedAt: at.Add(time.Second),
	}
	validFail := &domain.VideoValidation{
		Duration:  domain.ShortsDuration(30 * time.Second),
		Width:     1080,
		Height:    1920,
		HasAudio:  true,
		Passed:    false,
		Reason:    "black frames",
		CheckedAt: at.Add(time.Second),
	}

	base := domain.VideoJobSnapshot{
		ID:             domain.VideoJobID("job"),
		DailyRunID:     domain.DailyRunID("run"),
		StoryID:        domain.StoryID("story"),
		IdempotencyKey: domain.IdempotencyKey("job/key"),
		MaxAttempts:    3,
		CreatedAt:      at,
		UpdatedAt:      at.Add(6 * time.Second),
		ValidatedAt:    at.Add(2 * time.Second),
		UploadedAt:     at.Add(3 * time.Second),
		ScheduledAt:    at.Add(4 * time.Second),
		PublishedAt:    at.Add(6 * time.Second),
	}

	cases := []struct {
		name string
		mod  func(s *domain.VideoJobSnapshot)
	}{
		{
			name: "published without validation",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StatePublished
				s.PublicationID = domain.PublicationID("pub-1")
			},
		},
		{
			name: "validated with failed validation",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StateValidated
				s.Validation = validFail
				s.UploadedAt = time.Time{}
				s.ScheduledAt = time.Time{}
				s.PublishedAt = time.Time{}
			},
		},
		{
			name: "published without publication id",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StatePublished
				s.Validation = validPass
			},
		},
		{
			name: "out of order lifecycle timestamps",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StatePublished
				s.Validation = validPass
				s.PublicationID = domain.PublicationID("pub-1")
				s.UploadedAt = at.Add(1 * time.Second) // before ValidatedAt
			},
		},
		{
			name: "retry pending with zero attempts",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StateRetryPending
				s.ResumeState = domain.StateScripted
				s.Attempts = 0
				s.ValidatedAt = time.Time{}
				s.UploadedAt = time.Time{}
				s.ScheduledAt = time.Time{}
				s.PublishedAt = time.Time{}
			},
		},
		{
			name: "retry pending with non-retryable resume state",
			mod: func(s *domain.VideoJobSnapshot) {
				s.State = domain.StateRetryPending
				s.ResumeState = domain.StatePublished
				s.Attempts = 1
				s.Validation = validPass
				s.PublicationID = domain.PublicationID("pub-1")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := base
			tc.mod(&snap)
			if job, err := domain.RehydrateVideoJob(snap); err == nil {
				t.Fatalf("expected error, got rehydrated job in state %s", job.State())
			}
		})
	}
}

func TestRehydrateVideoJobRoundtripReachableStates(t *testing.T) {
	at := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	newBase := func(t *testing.T) *domain.VideoJob {
		t.Helper()
		j, err := domain.NewVideoJob(domain.NewVideoJobParams{
			ID:             "job-rt",
			DailyRunID:     "run-rt",
			StoryID:        "story-rt",
			IdempotencyKey: "rt/key/1",
			MaxAttempts:    3,
			CreatedAt:      at,
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	assertRoundtrip := func(t *testing.T, j *domain.VideoJob) {
		t.Helper()
		snap := j.Snapshot()
		rehydrated, err := domain.RehydrateVideoJob(snap)
		if err != nil {
			t.Fatalf("RehydrateVideoJob(%s) error = %v", snap.State, err)
		}
		if rehydrated.State() != j.State() || rehydrated.ResumeState() != j.ResumeState() ||
			rehydrated.PublicationID() != j.PublicationID() || rehydrated.Attempts() != j.Attempts() {
			t.Fatalf("roundtrip mismatch: got=%+v want=%+v", rehydrated.Snapshot(), snap)
		}
	}

	// CREATED -> SCRIPTED -> MEDIA_READY -> RENDERED -> VALIDATED -> UPLOADED -> SCHEDULED -> PUBLISHED.
	j := newBase(t)
	assertRoundtrip(t, j)

	for i, st := range []domain.VideoState{domain.StateScripted, domain.StateMediaReady, domain.StateRendered} {
		if err := j.Transition(st, at.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
		assertRoundtrip(t, j)
	}

	if err := j.SetValidation(domain.VideoValidation{
		Duration:  domain.ShortsDuration(25 * time.Second),
		Width:     1080,
		Height:    1920,
		HasAudio:  true,
		Passed:    true,
		CheckedAt: at.Add(4 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := j.Transition(domain.StateValidated, at.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, j)

	if err := j.Transition(domain.StateUploaded, at.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, j)

	if err := j.AttachPublication("pub-rt", at.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, j)

	if err := j.Transition(domain.StateScheduled, at.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, j)

	if err := j.Transition(domain.StatePublished, at.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, j)

	// FAILED -> RETRY_PENDING -> resume -> FAILED without resumeState.
	jRetry := newBase(t)
	if err := jRetry.Transition(domain.StateScripted, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := jRetry.Transition(domain.StateFailed, at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, jRetry)

	if err := jRetry.Transition(domain.StateRetryPending, at.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, jRetry)

	if err := jRetry.Transition(domain.StateScripted, at.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, jRetry)

	// Legitimate FAILED snapshot with empty ResumeState.
	snapFailedNoResume := jRetry.Snapshot()
	snapFailedNoResume.State = domain.StateFailed
	snapFailedNoResume.ResumeState = ""
	if _, err := domain.RehydrateVideoJob(snapFailedNoResume); err != nil {
		t.Fatalf("legitimate FAILED without resumeState rejected: %v", err)
	}

	// REJECTED from CREATED.
	jRej := newBase(t)
	if err := jRej.Transition(domain.StateRejected, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertRoundtrip(t, jRej)

	// SCRIPTED -> FAILED -> REJECTED (preserves ResumeState).
	jFailedRej := newBase(t)
	for i, st := range []domain.VideoState{domain.StateScripted, domain.StateFailed, domain.StateRejected} {
		if err := jFailedRej.Transition(st, at.Add(time.Duration(i+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	assertRoundtrip(t, jFailedRej)
}
