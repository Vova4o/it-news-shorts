package youtube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Adapter implements the domain VideoPublisher on top of Client, so the domain
// keeps depending only on its own interface.
type Adapter struct {
	Client *Client
	// Now is the clock used for timestamps.
	Now func() time.Time
}

// NewAdapter wires a client into the domain port.
func NewAdapter(client *Client) *Adapter { return &Adapter{Client: client} }

func (a *Adapter) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

// Upload publishes one rendered short. Test uploads stay private unless the
// request explicitly asks for something else.
func (a *Adapter) Upload(ctx context.Context, req UploadRequest) (domain.Publication, error) {
	if err := req.Validate(); err != nil {
		return domain.Publication{}, err
	}
	if a.Client == nil {
		return domain.Publication{}, fmt.Errorf("youtube: client is not configured")
	}
	path, err := localPath(req.Asset.URI)
	if err != nil {
		return domain.Publication{}, err
	}

	key, err := idempotencyKeyFor(req)
	if err != nil {
		return domain.Publication{}, err
	}

	input := UploadInput{
		FilePath:       path,
		Title:          req.Title,
		Description:    req.Description,
		Tags:           req.Tags,
		Language:       string(req.Language),
		Privacy:        req.Privacy,
		IdempotencyKey: key,
		VideoJobID:     string(req.VideoJobID),
	}.Normalize()

	result, err := a.Client.UploadVideo(ctx, input)
	if err != nil {
		return domain.Publication{}, err
	}
	createdAt := result.UploadedAt
	if createdAt.IsZero() {
		createdAt = a.now()
	}
	return domain.Publication{
		ID:             domain.PublicationID("pub-" + result.VideoID),
		VideoJobID:     req.VideoJobID,
		YouTubeVideoID: result.VideoID,
		Status:         domain.PublicationPending,
		IdempotencyKey: domain.IdempotencyKey(key),
		CreatedAt:      createdAt,
	}, nil
}

// Schedule sets the publish time of an already uploaded video. It verifies the
// publication's upload record in the journal before sending any mutating API
// request.
func (a *Adapter) Schedule(ctx context.Context, req ScheduleRequest) (domain.Publication, error) {
	if err := req.Validate(); err != nil {
		return domain.Publication{}, err
	}
	if a.Client == nil || a.Client.journal == nil {
		return domain.Publication{}, fmt.Errorf("youtube: upload journal is required to schedule publication")
	}
	videoID := strings.TrimPrefix(string(req.PublicationID), "pub-")
	if videoID == "" || videoID == string(req.PublicationID) {
		return domain.Publication{}, fmt.Errorf("youtube: publication %q does not carry a video id", req.PublicationID)
	}

	entry, found, err := a.Client.journal.FindByVideoID(ctx, videoID)
	if err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: lookup publication %q: %w", req.PublicationID, err)
	}
	if !found {
		return domain.Publication{}, fmt.Errorf("youtube: unknown publication %q: no upload record for video %s", req.PublicationID, videoID)
	}
	if strings.TrimSpace(entry.VideoJobID) == "" {
		return domain.Publication{}, fmt.Errorf("youtube: publication %q upload record is missing video_job_id", req.PublicationID)
	}
	key := domain.IdempotencyKey(entry.Key)
	if err := key.Validate(); err != nil {
		return domain.Publication{}, fmt.Errorf("youtube: publication %q has invalid idempotency key: %w", req.PublicationID, err)
	}
	if entry.Privacy == string(PrivacyPublic) {
		return domain.Publication{}, fmt.Errorf("%w: %s is already recorded as public", ErrAlreadyPublished, videoID)
	}

	plan, err := a.Client.SchedulePublication(ctx, videoID, req.PublishAt, false)
	if err != nil {
		return domain.Publication{}, err
	}
	createdAt := entry.UploadedAt
	if createdAt.IsZero() {
		createdAt = entry.CreatedAt
	}
	if createdAt.IsZero() {
		createdAt = a.now()
	}
	return domain.Publication{
		ID:             req.PublicationID,
		VideoJobID:     domain.VideoJobID(entry.VideoJobID),
		YouTubeVideoID: plan.VideoID,
		Status:         domain.PublicationScheduled,
		IdempotencyKey: key,
		ScheduledAt:    plan.PublishAt,
		CreatedAt:      createdAt,
	}, nil
}

// localPath turns a media asset URI into a filesystem path.
func localPath(uri string) (string, error) {
	trimmed := strings.TrimSpace(uri)
	if trimmed == "" {
		return "", fmt.Errorf("youtube: media asset has no URI")
	}
	if strings.HasPrefix(trimmed, "file://") {
		parsed, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("youtube: parse media asset URI: %w", err)
		}
		return parsed.Path, nil
	}
	if strings.Contains(trimmed, "://") {
		return "", fmt.Errorf("youtube: media asset URI %q is not a local file; download it first", trimmed)
	}
	return trimmed, nil
}

// idempotencyKeyFor builds an unambiguous key that satisfies
// domain.IdempotencyKey rules (letters, digits, and - _ : . /, max 128 chars)
// by hashing length-prefixed raw VideoJobID and Asset.ID components.
func idempotencyKeyFor(req UploadRequest) (string, error) {
	job := string(req.VideoJobID)
	asset := string(req.Asset.ID)
	if strings.TrimSpace(job) == "" {
		return "", fmt.Errorf("youtube: video job id cannot form an idempotency key")
	}
	payload := fmt.Sprintf("v1:%d:%s:%d:%s", len(job), job, len(asset), asset)
	sum := sha256.Sum256([]byte(payload))
	key := "yt/v1/" + hex.EncodeToString(sum[:24])
	if err := (domain.IdempotencyKey(key)).Validate(); err != nil {
		return "", err
	}
	return key, nil
}
