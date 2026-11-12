package script

import (
	"context"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// ScriptRequest is the input of one script generation.
type ScriptRequest struct {
	VideoJobID     domain.VideoJobID
	Story          domain.Story
	TargetDuration domain.ShortsDuration
	Language       domain.Language
}

// Validate enforces the request rules.
func (r ScriptRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("script_request.video_job_id")
	}
	if err := r.Story.Validate(); err != nil {
		return err
	}
	if err := r.TargetDuration.Validate(); err != nil {
		return err
	}
	if err := r.Language.Validate(); err != nil {
		return err
	}
	return nil
}

// ScriptGenerator turns a verified story into a Russian narration script.
type ScriptGenerator interface {
	Generate(ctx context.Context, req ScriptRequest) (domain.Script, error)
}
