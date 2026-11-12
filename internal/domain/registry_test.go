package domain_test

import (
	"errors"
	"testing"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

func TestRegistryRejectsDuplicateIDs(t *testing.T) {
	registry := domain.NewRegistry()
	if err := registry.RegisterID("video_job", "vj-01"); err != nil {
		t.Fatalf("RegisterID() error = %v", err)
	}
	if !registry.HasID("video_job", "vj-01") {
		t.Fatal("HasID() = false after registration")
	}
	if err := registry.RegisterID("video_job", "vj-01"); !errors.Is(err, domain.ErrDuplicateID) {
		t.Fatalf("duplicate error = %v, want %v", err, domain.ErrDuplicateID)
	}
	if err := registry.RegisterID("story", "vj-01"); err != nil {
		t.Fatalf("same id in another kind must be allowed, got %v", err)
	}
	if err := registry.RegisterID("video_job", ""); !errors.Is(err, domain.ErrMissingField) {
		t.Fatalf("empty id error = %v, want %v", err, domain.ErrMissingField)
	}
}

func TestRegistryRejectsDuplicateIdempotencyKeys(t *testing.T) {
	registry := domain.NewRegistry()
	key := domain.IdempotencyKey("run-2026-11-12/story-01/publish")
	if err := registry.RegisterIdempotencyKey(key); err != nil {
		t.Fatalf("RegisterIdempotencyKey() error = %v", err)
	}
	if !registry.HasIdempotencyKey(key) {
		t.Fatal("HasIdempotencyKey() = false after registration")
	}
	if err := registry.RegisterIdempotencyKey(key); !errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate error = %v, want %v", err, domain.ErrDuplicateIdempotencyKey)
	}
	if err := registry.RegisterIdempotencyKey(""); !errors.Is(err, domain.ErrMissingField) {
		t.Fatalf("empty key error = %v, want %v", err, domain.ErrMissingField)
	}
}

func TestIdentifierConstructors(t *testing.T) {
	constructors := []struct {
		name string
		new  func(string) (string, error)
	}{
		{"article", func(raw string) (string, error) { id, err := domain.NewArticleID(raw); return string(id), err }},
		{"story", func(raw string) (string, error) { id, err := domain.NewStoryID(raw); return string(id), err }},
		{"daily run", func(raw string) (string, error) { id, err := domain.NewDailyRunID(raw); return string(id), err }},
		{"video job", func(raw string) (string, error) { id, err := domain.NewVideoJobID(raw); return string(id), err }},
		{"script", func(raw string) (string, error) { id, err := domain.NewScriptID(raw); return string(id), err }},
		{"media asset", func(raw string) (string, error) { id, err := domain.NewMediaAssetID(raw); return string(id), err }},
		{"publication", func(raw string) (string, error) { id, err := domain.NewPublicationID(raw); return string(id), err }},
		{"cost", func(raw string) (string, error) { id, err := domain.NewCostID(raw); return string(id), err }},
	}
	inputs := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{"valid", "  id-01  ", "id-01", nil},
		{"empty", "", "", domain.ErrMissingField},
		{"blank", "   ", "", domain.ErrMissingField},
		{"with space", "id 01", "", domain.ErrInvalidValue},
	}
	for _, constructor := range constructors {
		for _, input := range inputs {
			t.Run(constructor.name+"/"+input.name, func(t *testing.T) {
				got, err := constructor.new(input.raw)
				if input.wantErr != nil {
					if !errors.Is(err, input.wantErr) {
						t.Fatalf("error = %v, want %v", err, input.wantErr)
					}
					if got != "" {
						t.Fatalf("id = %q, want empty", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				if got != input.want {
					t.Fatalf("id = %q, want %q", got, input.want)
				}
			})
		}
	}
}

func TestIdempotencyKeyFormat(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr error
	}{
		{"daily key", "run-2026-11-12/story-01/publish", nil},
		{"with colon and dot", "video:vj-01/part.1", nil},
		{"empty", "", domain.ErrMissingField},
		{"space", "run 1", domain.ErrInvalidValue},
		{"cyrillic", "ключ", domain.ErrInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := domain.NewIdempotencyKey(tt.raw)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if key.String() != tt.raw {
				t.Fatalf("key = %q, want %q", key.String(), tt.raw)
			}
		})
	}
}
