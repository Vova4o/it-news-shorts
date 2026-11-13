package news

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHackerNewsSource_FetchAndFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/topstories.json":
			_, _ = w.Write([]byte(`[1, 2, 3, 4, 5, 6, 7]`))
		case "/newstories.json":
			_, _ = w.Write([]byte(`[1, 8]`))
		case "/item/1.json":
			_, _ = w.Write([]byte(`{"id":1,"type":"story","by":"pg","time":1794556800,"title":"Valid Story","url":"https://example.com/story?utm_source=hn","score":120,"descendants":45}`))
		case "/item/2.json":
			_, _ = w.Write([]byte(`{"id":2,"type":"story","deleted":true,"title":"Deleted"}`))
		case "/item/3.json":
			_, _ = w.Write([]byte(`{"id":3,"type":"story","dead":true,"title":"Dead"}`))
		case "/item/4.json":
			_, _ = w.Write([]byte(`{"id":4,"type":"job","title":"Hiring Go Dev"}`))
		case "/item/5.json":
			_, _ = w.Write([]byte(`{"id":5,"type":"comment","text":"nice"}`))
		case "/item/6.json":
			_, _ = w.Write([]byte(`null`))
		case "/item/7.json":
			_, _ = w.Write([]byte(`{"id":7,"type":"story","by":"ask","time":1794556800,"title":"Ask HN: Best Go libs?"}`))
		case "/item/8.json":
			_, _ = w.Write([]byte(`{invalid-json`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	hn, err := NewHackerNewsSource(HackerNewsOptions{
		BaseURL:        srv.URL,
		Lists:          []string{"top", "new"},
		LimitPerList:   10,
		MaxConcurrency: 3,
	}, testHTTPClient(t))
	if err != nil {
		t.Fatalf("NewHackerNewsSource() error = %v", err)
	}

	res, err := hn.Fetch(context.Background(), FetchOptions{Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	// Expect item 1 and item 7 (Ask HN falls back to HN discussion URL).
	if len(res.Articles) != 2 {
		t.Fatalf("len(Articles) = %d, want 2 (%+v)", len(res.Articles), res.Articles)
	}
	if res.Articles[0].ExternalID != "1" || res.Articles[0].CanonicalURL != "https://example.com/story" {
		t.Fatalf("unexpected item 1: %+v", res.Articles[0])
	}
	if res.Articles[0].PublishedAt != nil {
		t.Fatalf("expected PublishedAt=nil for HN-only item, got %v", res.Articles[0].PublishedAt)
	}
	if res.Articles[0].SourceSubmittedAt == nil {
		t.Fatal("expected SourceSubmittedAt to be set for HN item")
	}
	if res.Articles[0].PopularityScore == nil || *res.Articles[0].PopularityScore != 120 {
		t.Fatalf("unexpected score: %v", res.Articles[0].PopularityScore)
	}
	if res.Articles[1].ExternalID != "7" || res.Articles[1].CanonicalURL != "https://news.ycombinator.com/item?id=7" {
		t.Fatalf("unexpected Ask HN fallback URL: %+v", res.Articles[1])
	}
}

func TestHackerNewsSource_HTTPErrorAndRetry429(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/topstories.json" {
			_, _ = w.Write([]byte(`[1]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":1,"type":"story","title":"Recovered","url":"https://example.com/ok","time":1794556800}`))
	}))
	defer srv.Close()

	hn, err := NewHackerNewsSource(HackerNewsOptions{
		BaseURL:      srv.URL,
		Lists:        []string{"top"},
		LimitPerList: 5,
	}, testHTTPClient(t))
	if err != nil {
		t.Fatalf("NewHackerNewsSource() error = %v", err)
	}

	res, err := hn.Fetch(context.Background(), FetchOptions{})
	if err != nil {
		t.Fatalf("expected recovery after 429 retry, got error = %v", err)
	}
	if len(res.Articles) != 1 || res.Articles[0].Title != "Recovered" {
		t.Fatalf("unexpected articles after retry: %+v", res.Articles)
	}
}
