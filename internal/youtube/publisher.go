package youtube

import (
	"context"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// PrivacyStatus is the YouTube visibility of an upload.
type PrivacyStatus string

// Visibility values.
const (
	PrivacyPrivate  PrivacyStatus = "private"
	PrivacyUnlisted PrivacyStatus = "unlisted"
	PrivacyPublic   PrivacyStatus = "public"
)

// Valid reports whether the visibility is known.
func (p PrivacyStatus) Valid() bool {
	switch p {
	case PrivacyPrivate, PrivacyUnlisted, PrivacyPublic:
		return true
	default:
		return false
	}
}

// UploadRequest is the input of one upload.
type UploadRequest struct {
	VideoJobID  domain.VideoJobID
	Asset       domain.MediaAsset
	Title       string
	Description string
	Tags        []string
	Privacy     PrivacyStatus
	Language    domain.Language
}

// Validate enforces the YouTube metadata limits.
func (r UploadRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("upload_request.video_job_id")
	}
	if err := r.Asset.Validate(); err != nil {
		return err
	}
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return domain.MissingField("upload_request.title")
	}
	if len([]rune(title)) > 100 {
		return domain.InvalidValue("upload_request.title")
	}
	if !r.Privacy.Valid() {
		return domain.InvalidValue("upload_request.privacy")
	}
	if err := r.Language.Validate(); err != nil {
		return err
	}
	return nil
}

// ScheduleRequest is the input of one scheduling call.
type ScheduleRequest struct {
	PublicationID domain.PublicationID
	PublishAt     time.Time
}

// Validate enforces the request rules.
func (r ScheduleRequest) Validate() error {
	if r.PublicationID == "" {
		return domain.MissingField("schedule_request.publication_id")
	}
	if r.PublishAt.IsZero() {
		return domain.MissingField("schedule_request.publish_at")
	}
	return nil
}

// VideoPublisher uploads and schedules shorts. Real OAuth arrives in TASK 002.
type VideoPublisher interface {
	Upload(ctx context.Context, req UploadRequest) (domain.Publication, error)
	Schedule(ctx context.Context, req ScheduleRequest) (domain.Publication, error)
}
