package video

import (
	"context"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Resolution is the pixel size of a render target.
type Resolution struct {
	Width  int
	Height int
}

// ShortsResolution is the vertical 1080x1920 target.
var ShortsResolution = Resolution{Width: 1080, Height: 1920}

// Validate enforces the resolution rules.
func (r Resolution) Validate() error {
	if r.Width <= 0 {
		return domain.InvalidValue("resolution.width")
	}
	if r.Height <= 0 {
		return domain.InvalidValue("resolution.height")
	}
	if r.Width >= r.Height {
		return domain.InvalidValue("resolution.orientation")
	}
	return nil
}

// ClipRequest is the input of one generated visual clip.
type ClipRequest struct {
	VideoJobID domain.VideoJobID
	Prompt     string
	Duration   domain.ShortsDuration
	Resolution Resolution
}

// Validate enforces the request rules.
func (r ClipRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("clip_request.video_job_id")
	}
	if r.Prompt == "" {
		return domain.MissingField("clip_request.prompt")
	}
	if err := r.Duration.Validate(); err != nil {
		return err
	}
	return r.Resolution.Validate()
}

// VideoGenerationProvider produces AI generated visual material.
type VideoGenerationProvider interface {
	GenerateClip(ctx context.Context, req ClipRequest) (domain.MediaAsset, error)
}

// RenderRequest is the input of the FFmpeg render step.
type RenderRequest struct {
	VideoJobID domain.VideoJobID
	Script     domain.Script
	Assets     []domain.MediaAsset
	Duration   domain.ShortsDuration
	Resolution Resolution
}

// Validate enforces the request rules.
func (r RenderRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("render_request.video_job_id")
	}
	if err := r.Script.Validate(); err != nil {
		return err
	}
	if len(r.Assets) == 0 {
		return domain.MissingField("render_request.assets")
	}
	for _, asset := range r.Assets {
		if err := asset.Validate(); err != nil {
			return err
		}
	}
	if err := r.Duration.Validate(); err != nil {
		return err
	}
	return r.Resolution.Validate()
}

// VideoRenderer combines narration, visuals and music into a short.
type VideoRenderer interface {
	Render(ctx context.Context, req RenderRequest) (domain.MediaAsset, error)
}

// ValidateRequest is the input of the technical validation step.
type ValidateRequest struct {
	VideoJobID  domain.VideoJobID
	Asset       domain.MediaAsset
	MinDuration time.Duration
	MaxDuration time.Duration
}

// Validate enforces the request rules.
func (r ValidateRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("validate_request.video_job_id")
	}
	if err := r.Asset.Validate(); err != nil {
		return err
	}
	if r.MinDuration <= 0 {
		return domain.InvalidValue("validate_request.min_duration")
	}
	if r.MaxDuration < r.MinDuration {
		return domain.InvalidValue("validate_request.max_duration")
	}
	return nil
}

// VideoValidator checks duration, audio track and resolution before upload.
type VideoValidator interface {
	Validate(ctx context.Context, req ValidateRequest) (domain.VideoValidation, error)
}
