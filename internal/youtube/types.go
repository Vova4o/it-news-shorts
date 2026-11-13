package youtube

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Integration errors. They are wrapped, so callers should match with errors.Is.
var (
	ErrCredentialsMissing  = errors.New("youtube: credentials are not configured")
	ErrRefreshTokenInvalid = errors.New("youtube: refresh token is invalid, expired or revoked")
	ErrAuthorizationDenied = errors.New("youtube: authorization was denied")
	ErrStateMismatch       = errors.New("youtube: oauth state mismatch")
	ErrChannelMismatch     = errors.New("youtube: authorized channel does not match the expected one")
	ErrVideoNotFound       = errors.New("youtube: video not found")
	ErrInsufficientScope   = errors.New("youtube: token lacks the required scope")
	ErrAlreadyPublished    = errors.New("youtube: video is already published")
	ErrPublishAtNotFuture  = errors.New("youtube: publish time must be in the future")
	ErrVideoNotPrivate     = errors.New("youtube: video must be private before scheduling")
	ErrUploadUnknown       = errors.New("youtube: upload result is unknown")
	ErrRetriesExhausted    = errors.New("youtube: retries exhausted")
	ErrSessionLost         = errors.New("youtube: resumable session is no longer valid")
)

// UploadState is the local lifecycle of one upload operation.
type UploadState string

// Upload states kept in the idempotency journal.
const (
	UploadStatePending    UploadState = "PENDING"
	UploadStateInProgress UploadState = "IN_PROGRESS"
	UploadStateCompleted  UploadState = "COMPLETED"
	UploadStateFailed     UploadState = "FAILED"
	UploadStateUnknown    UploadState = "UNKNOWN"
)

// Terminal reports whether no automatic retry should follow.
func (s UploadState) Terminal() bool {
	return s == UploadStateCompleted || s == UploadStateFailed || s == UploadStateUnknown
}

// Channel is the YouTube channel the credentials belong to.
type Channel struct {
	ID    string
	Title string
}

// UploadInput describes one resumable upload.
type UploadInput struct {
	// FilePath is the local MP4 to upload.
	FilePath string
	// Title is the video title, at most 100 characters.
	Title string
	// Description is the video description, at most 5000 characters.
	Description string
	// Tags are the video tags.
	Tags []string
	// CategoryID is the YouTube category, for example 28 for Science & Technology.
	CategoryID string
	// Language is the BCP-47 language of the video, for example "ru".
	Language string
	// MadeForKids is the audience designation sent as selfDeclaredMadeForKids.
	MadeForKids bool
	// SyntheticMedia marks the video as altered or synthetic content.
	SyntheticMedia bool
	// IdempotencyKey is our own key; YouTube does not deduplicate by it.
	IdempotencyKey string
	// Privacy is the requested visibility. Empty means private.
	Privacy PrivacyStatus
	// VideoJobID is an optional domain job identifier recorded in the journal.
	VideoJobID string
}

// DefaultCategoryID is Science & Technology.
const DefaultCategoryID = "28"

// Normalize fills the defaults and trims the input.
func (in UploadInput) Normalize() UploadInput {
	if in.Privacy == "" {
		in.Privacy = PrivacyPrivate
	}
	if strings.TrimSpace(in.CategoryID) == "" {
		in.CategoryID = DefaultCategoryID
	}
	return in
}

// Validate enforces the metadata limits of the Data API.
func (in UploadInput) Validate() error {
	if strings.TrimSpace(in.FilePath) == "" {
		return errors.New("youtube: upload file path is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return errors.New("youtube: upload title is required")
	}
	if len([]rune(title)) > 100 {
		return fmt.Errorf("youtube: upload title is longer than 100 characters")
	}
	if len([]rune(in.Description)) > 5000 {
		return fmt.Errorf("youtube: upload description is longer than 5000 characters")
	}
	if !in.Privacy.Valid() {
		return fmt.Errorf("youtube: unknown privacy status %q", in.Privacy)
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return errors.New("youtube: idempotency key is required")
	}
	return nil
}

// UploadResult is the outcome of one upload.
type UploadResult struct {
	VideoID       string
	URL           string
	UploadStatus  string
	PrivacyStatus string
	UploadedAt    time.Time
	State         UploadState
	Resumed       bool
}

// VideoURL returns the public watch URL of the video.
func VideoURL(videoID string) string {
	if videoID == "" {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + videoID
}

// VideoStatus is the state of one uploaded video as reported by videos.list.
type VideoStatus struct {
	VideoID          string
	Title            string
	PrivacyStatus    string
	UploadStatus     string
	ProcessingStatus string
	PublishAt        time.Time
	FailureReason    string
	RejectionReason  string
}

// Processed reports whether YouTube finished processing the video.
func (s VideoStatus) Processed() bool {
	return s.ProcessingStatus == "succeeded"
}

// SchedulePlan describes what SchedulePublication would send.
type SchedulePlan struct {
	VideoID   string
	PublishAt time.Time
	Privacy   PrivacyStatus
	DryRun    bool
	Request   string
}

// Scheduled reports whether the request was actually sent.
func (p SchedulePlan) Scheduled() bool { return !p.DryRun }
