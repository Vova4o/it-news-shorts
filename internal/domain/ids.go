package domain

import (
	"strings"
	"unicode"
)

// Every aggregate owns its own identifier type so that identifiers of
// different aggregates cannot be mixed up at compile time.
type (
	ArticleID     string
	StoryID       string
	DailyRunID    string
	VideoJobID    string
	ScriptID      string
	MediaAssetID  string
	PublicationID string
	CostID        string
)

// IdempotencyKey guards every external side effect against duplicates.
type IdempotencyKey string

const maxIdempotencyKeyLength = 128

func newID[T ~string](raw string) (T, error) {
	var zero T
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return zero, MissingField("id")
	}
	for _, r := range trimmed {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return zero, InvalidValue("id")
		}
	}
	return T(trimmed), nil
}

// NewArticleID validates and converts a raw article identifier.
func NewArticleID(raw string) (ArticleID, error) { return newID[ArticleID](raw) }

// NewStoryID validates and converts a raw story identifier.
func NewStoryID(raw string) (StoryID, error) { return newID[StoryID](raw) }

// NewDailyRunID validates and converts a raw daily run identifier.
func NewDailyRunID(raw string) (DailyRunID, error) { return newID[DailyRunID](raw) }

// NewVideoJobID validates and converts a raw video job identifier.
func NewVideoJobID(raw string) (VideoJobID, error) { return newID[VideoJobID](raw) }

// NewScriptID validates and converts a raw script identifier.
func NewScriptID(raw string) (ScriptID, error) { return newID[ScriptID](raw) }

// NewMediaAssetID validates and converts a raw media asset identifier.
func NewMediaAssetID(raw string) (MediaAssetID, error) { return newID[MediaAssetID](raw) }

// NewPublicationID validates and converts a raw publication identifier.
func NewPublicationID(raw string) (PublicationID, error) { return newID[PublicationID](raw) }

// NewCostID validates and converts a raw cost identifier.
func NewCostID(raw string) (CostID, error) { return newID[CostID](raw) }

func (id ArticleID) String() string     { return string(id) }
func (id StoryID) String() string       { return string(id) }
func (id DailyRunID) String() string    { return string(id) }
func (id VideoJobID) String() string    { return string(id) }
func (id ScriptID) String() string      { return string(id) }
func (id MediaAssetID) String() string  { return string(id) }
func (id PublicationID) String() string { return string(id) }
func (id CostID) String() string        { return string(id) }

// NewIdempotencyKey validates a deterministic key such as
// "run-2026-11-12/story-01/v1".
func NewIdempotencyKey(raw string) (IdempotencyKey, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", MissingField("idempotency_key")
	}
	if len(trimmed) > maxIdempotencyKeyLength {
		return "", InvalidValue("idempotency_key")
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == ':', r == '.', r == '/':
		default:
			return "", InvalidValue("idempotency_key")
		}
	}
	return IdempotencyKey(trimmed), nil
}

func (k IdempotencyKey) String() string { return string(k) }

// Validate reports whether the key is usable.
func (k IdempotencyKey) Validate() error {
	_, err := NewIdempotencyKey(string(k))
	return err
}
