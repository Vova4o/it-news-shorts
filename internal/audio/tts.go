package audio

import (
	"context"
	"strings"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// SpeechRequest is the input of one text to speech call.
type SpeechRequest struct {
	VideoJobID domain.VideoJobID
	ScriptID   domain.ScriptID
	Text       string
	Language   domain.Language
	VoiceID    string
}

// Validate enforces the request rules.
func (r SpeechRequest) Validate() error {
	if r.VideoJobID == "" {
		return domain.MissingField("speech_request.video_job_id")
	}
	if r.ScriptID == "" {
		return domain.MissingField("speech_request.script_id")
	}
	if strings.TrimSpace(r.Text) == "" {
		return domain.MissingField("speech_request.text")
	}
	if err := r.Language.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.VoiceID) == "" {
		return domain.MissingField("speech_request.voice_id")
	}
	return nil
}

// TTSProvider synthesizes narration audio.
type TTSProvider interface {
	Synthesize(ctx context.Context, req SpeechRequest) (domain.MediaAsset, error)
}
