package news_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

func TestSampleArticlesAreValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "articles.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var articles []domain.Article
	if err := json.Unmarshal(raw, &articles); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(articles) == 0 {
		t.Fatal("fixture contains no articles")
	}
	ids := domain.NewRegistry()
	for _, article := range articles {
		if err := article.Validate(); err != nil {
			t.Fatalf("article %s: %v", article.ID, err)
		}
		if err := ids.RegisterID("article", article.ID.String()); err != nil {
			t.Fatalf("article %s: %v", article.ID, err)
		}
	}
}

func TestSourceValidation(t *testing.T) {
	tests := []struct {
		name    string
		source  news.Source
		wantErr error
	}{
		{"valid", news.Source{Name: "Example", FeedURL: "https://example.com/rss", Language: domain.LanguageEnglish}, nil},
		{"missing name", news.Source{FeedURL: "https://example.com/rss", Language: domain.LanguageEnglish}, domain.ErrMissingField},
		{"missing feed", news.Source{Name: "Example", Language: domain.LanguageEnglish}, domain.ErrMissingField},
		{"relative feed", news.Source{Name: "Example", FeedURL: "example.com/rss", Language: domain.LanguageEnglish}, domain.ErrInvalidValue},
		{"unknown language", news.Source{Name: "Example", FeedURL: "https://example.com/rss", Language: "xx"}, domain.ErrInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.source.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
