package news

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"golang.org/x/text/encoding/charmap"
)

func testHTTPClient(t *testing.T) *HTTPClient {
	t.Helper()
	return NewHTTPClient(HTTPClientOptions{
		Timeout:         5 * time.Second,
		MaxBytes:        1 << 20,
		MaxConcurrency:  4,
		MaxRetries:      1,
		BaseBackoff:     10 * time.Millisecond,
		AllowPrivateIPs: true,
	})
}

func TestParseFeed_RSS20(t *testing.T) {
	xmlData := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:content="http://purl.org/rss/1.0/modules/content/">
  <channel>
    <title>Tech News</title>
    <item>
      <title>Go &amp; Temporal &lt;b&gt;Update&lt;/b&gt;</title>
      <link>https://example.com/post?id=42&amp;utm_source=rss#top</link>
      <guid>post-42</guid>
      <description>&lt;p&gt;Hello &amp;amp; world&lt;/p&gt;&lt;script&gt;alert(1)&lt;/script&gt;</description>
      <pubDate>Fri, 13 Nov 2026 08:00:00 +0300</pubDate>
      <dc:creator>Alice</dc:creator>
      <category>Go</category>
      <unknownField>ignored</unknownField>
    </item>
    <item>
      <title>   </title>
      <link>https://example.com/no-title</link>
    </item>
    <item>
      <title>Missing Link</title>
    </item>
  </channel>
</rss>`)

	cfg := SourceConfig{
		ID:       "tech",
		Name:     "Tech",
		Type:     SourceTypeRSS,
		FeedURL:  "https://example.com/feed",
		Language: domain.LanguageEnglish,
		Enabled:  true,
	}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	articles, total, err := ParseFeed(xmlData, cfg, now, 10)
	if err != nil {
		t.Fatalf("ParseFeed() error = %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if len(articles) != 1 {
		t.Fatalf("len(articles) = %d, want 1", len(articles))
	}
	a := articles[0]
	if a.Title != "Go & Temporal Update" {
		t.Fatalf("Title = %q, want %q", a.Title, "Go & Temporal Update")
	}
	if a.Description != "Hello & world" {
		t.Fatalf("Description = %q, want %q", a.Description, "Hello & world")
	}
	if a.CanonicalURL != "https://example.com/post?id=42" {
		t.Fatalf("CanonicalURL = %q", a.CanonicalURL)
	}
	if a.Author != "Alice" {
		t.Fatalf("Author = %q", a.Author)
	}
	if a.PublishedAt == nil || !a.PublishedAt.Equal(time.Date(2026, 11, 13, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("PublishedAt = %v, want 2026-11-13T05:00:00Z", a.PublishedAt)
	}
}

func TestParseFeed_Atom(t *testing.T) {
	xmlData := []byte(`<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>The Verge</title>
  <entry>
    <id>tag:theverge.com,2026:1</id>
    <title>Atom Story</title>
    <link rel="alternate" type="text/html" href="https://www.theverge.com/2026/11/13/story"/>
    <published>2026-11-13T04:30:00Z</published>
    <author><name>Bob</name></author>
    <summary type="html">&lt;p&gt;Summary text&lt;/p&gt;</summary>
    <category term="AI"/>
  </entry>
</feed>`)

	cfg := SourceConfig{
		ID:       "theverge",
		Name:     "The Verge",
		Type:     SourceTypeAtom,
		FeedURL:  "https://www.theverge.com/rss/index.xml",
		Language: domain.LanguageEnglish,
		Enabled:  true,
	}
	articles, _, err := ParseFeed(xmlData, cfg, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ParseFeed() error = %v", err)
	}
	if len(articles) != 1 {
		t.Fatalf("len(articles) = %d, want 1", len(articles))
	}
	if articles[0].Author != "Bob" || articles[0].Description != "Summary text" {
		t.Fatalf("unexpected atom article: %+v", articles[0])
	}
}

func TestParseFeed_EmptyAndMalformed(t *testing.T) {
	cfg := SourceConfig{
		ID: "s", Name: "S", Type: SourceTypeRSS, FeedURL: "https://example.com/rss", Language: domain.LanguageEnglish,
	}
	if _, _, err := ParseFeed([]byte("   "), cfg, time.Now(), 10); err == nil {
		t.Fatal("expected error for empty feed")
	}
	if _, _, err := ParseFeed([]byte("<rss><channel><item><title>broken"), cfg, time.Now(), 10); err == nil {
		t.Fatal("expected error for malformed xml")
	}
}

func TestParseFeed_Windows1251Charset(t *testing.T) {
	utf8XML := `<?xml version="1.0" encoding="windows-1251"?>
<rss version="2.0"><channel><item>
  <title>Привет мир</title>
  <link>https://example.ru/news/1</link>
  <pubDate>Fri, 13 Nov 2026 06:00:00 +0000</pubDate>
</item></channel></rss>`

	encoded, err := charmap.Windows1251.NewEncoder().Bytes([]byte(utf8XML))
	if err != nil {
		t.Fatalf("encode windows-1251: %v", err)
	}

	cfg := SourceConfig{
		ID: "ru", Name: "RU", Type: SourceTypeRSS, FeedURL: "https://example.ru/rss", Language: domain.LanguageRussian,
	}
	articles, _, err := ParseFeed(encoded, cfg, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ParseFeed() error = %v", err)
	}
	if len(articles) != 1 || articles[0].Title != "Привет мир" {
		t.Fatalf("unexpected decoded title: %+v", articles)
	}
}

func TestRSSSource_Conditional304(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Fri, 13 Nov 2026 06:00:00 GMT")
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><item><title>One</title><link>https://example.com/1</link><pubDate>Fri, 13 Nov 2026 06:00:00 GMT</pubDate></item></channel></rss>`))
	}))
	defer srv.Close()

	src, err := NewRSSSource(SourceConfig{
		ID: "srv", Name: "Srv", Type: SourceTypeRSS, FeedURL: srv.URL, Language: domain.LanguageEnglish, Enabled: true,
	}, testHTTPClient(t))
	if err != nil {
		t.Fatalf("NewRSSSource() error = %v", err)
	}

	res1, err := src.Fetch(context.Background(), FetchOptions{})
	if err != nil {
		t.Fatalf("first Fetch() error = %v", err)
	}
	if res1.NotModified || len(res1.Articles) != 1 || res1.ETag != `"v1"` {
		t.Fatalf("unexpected first FetchResult: %+v", res1)
	}

	res2, err := src.Fetch(context.Background(), FetchOptions{ETag: res1.ETag, LastModified: res1.LastModified})
	if err != nil {
		t.Fatalf("second Fetch() error = %v", err)
	}
	if !res2.NotModified || len(res2.Articles) != 0 {
		t.Fatalf("expected NotModified=true on second fetch, got %+v", res2)
	}
}
