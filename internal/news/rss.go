package news

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

// RSSSource fetches and parses RSS 2.0 and Atom feeds.
type RSSSource struct {
	cfg    SourceConfig
	client *HTTPClient
}

// NewRSSSource creates an RSS/Atom NewsSource adapter.
func NewRSSSource(cfg SourceConfig, client *HTTPClient) (*RSSSource, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, fmt.Errorf("news: http client is required")
	}
	return &RSSSource{cfg: cfg, client: client}, nil
}

// ID returns the configured source identifier.
func (s *RSSSource) ID() string {
	return s.cfg.ID
}

// Fetch downloads and parses the RSS 2.0 or Atom feed.
func (s *RSSSource) Fetch(ctx context.Context, opts FetchOptions) (FetchResult, error) {
	resp, err := s.client.Get(ctx, s.cfg.FeedURL, opts.ETag, opts.LastModified)
	if err != nil {
		return FetchResult{}, err
	}
	if resp.NotModified {
		return FetchResult{
			NotModified:  true,
			ETag:         opts.ETag,
			LastModified: opts.LastModified,
		}, nil
	}

	now := opts.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = s.cfg.Limit
	}

	articles, totalParsed, err := ParseFeed(resp.Body, s.cfg, now, limit)
	if err != nil {
		return FetchResult{}, fmt.Errorf("news: parse feed %s: %w", s.cfg.ID, err)
	}

	etag := resp.ETag
	if etag == "" {
		etag = opts.ETag
	}
	lastMod := resp.LastModified
	if lastMod == "" {
		lastMod = opts.LastModified
	}

	return FetchResult{
		Articles:     articles,
		ETag:         etag,
		LastModified: lastMod,
		FetchedCount: totalParsed,
	}, nil
}

type rssDocument struct {
	XMLName xml.Name   `xml:"rss"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title          string   `xml:"title"`
	Link           string   `xml:"link"`
	GUID           string   `xml:"guid"`
	Description    string   `xml:"description"`
	ContentEncoded string   `xml:"encoded"`
	PubDate        string   `xml:"pubDate"`
	DCDate         string   `xml:"date"`
	Author         string   `xml:"author"`
	Creator        string   `xml:"creator"`
	Categories     []string `xml:"category"`
}

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Title   string      `xml:"title"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Links      []atomLink     `xml:"link"`
	Summary    string         `xml:"summary"`
	Content    string         `xml:"content"`
	Published  string         `xml:"published"`
	Updated    string         `xml:"updated"`
	Author     atomAuthor     `xml:"author"`
	Categories []atomCategory `xml:"category"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomCategory struct {
	Term  string `xml:"term,attr"`
	Label string `xml:"label,attr"`
	Value string `xml:",chardata"`
}

// ParseFeed parses RSS 2.0 or Atom XML bytes into normalized UnifiedArticle items.
func ParseFeed(data []byte, cfg SourceConfig, fetchedAt time.Time, limit int) ([]UnifiedArticle, int, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, 0, fmt.Errorf("empty feed body")
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	} else {
		fetchedAt = fetchedAt.UTC()
	}

	rootName, err := detectXMLRoot(data)
	if err != nil {
		return nil, 0, err
	}

	switch rootName {
	case "rss":
		var doc rssDocument
		if err := decodeXMLWithCharset(data, &doc); err != nil {
			return nil, 0, err
		}
		return convertRSSItems(doc.Channel.Items, cfg, fetchedAt, limit), len(doc.Channel.Items), nil
	case "feed":
		var feed atomFeed
		if err := decodeXMLWithCharset(data, &feed); err != nil {
			return nil, 0, err
		}
		return convertAtomEntries(feed.Entries, cfg, fetchedAt, limit), len(feed.Entries), nil
	default:
		return nil, 0, fmt.Errorf("unsupported feed root element %q", rootName)
	}
}

func detectXMLRoot(data []byte) (string, error) {
	dec := newXMLDecoder(data)
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.ToLower(se.Name.Local), nil
		}
	}
}

func decodeXMLWithCharset(data []byte, dst any) error {
	dec := newXMLDecoder(data)
	return dec.Decode(dst)
}

func newXMLDecoder(data []byte) *xml.Decoder {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		enc, err := htmlindex.Get(charset)
		if err != nil {
			return nil, fmt.Errorf("unsupported charset %q: %w", charset, err)
		}
		return transform.NewReader(input, enc.NewDecoder()), nil
	}
	return dec
}

func convertRSSItems(items []rssItem, cfg SourceConfig, fetchedAt time.Time, limit int) []UnifiedArticle {
	var out []UnifiedArticle
	for _, item := range items {
		if limit > 0 && len(out) >= limit {
			break
		}
		desc := item.Description
		if strings.TrimSpace(desc) == "" {
			desc = item.ContentEncoded
		}
		author := item.Creator
		if strings.TrimSpace(author) == "" {
			author = item.Author
		}
		rawDate := item.PubDate
		if strings.TrimSpace(rawDate) == "" {
			rawDate = item.DCDate
		}
		extID := strings.TrimSpace(item.GUID)
		if extID == "" {
			extID = strings.TrimSpace(item.Link)
		}

		raw := UnifiedArticle{
			SourceID:     cfg.ID,
			ExternalID:   extID,
			Title:        item.Title,
			Description:  desc,
			OriginalURL:  item.Link,
			SourceType:   SourceTypeRSS,
			Author:       author,
			PublishedAt:  ParseDate(rawDate),
			DiscoveredAt: fetchedAt,
			FetchedAt:    fetchedAt,
			Language:     cfg.Language,
			Categories:   item.Categories,
			RawMetadata: map[string]any{
				"source_name": cfg.Name,
				"feed_url":    cfg.FeedURL,
			},
		}
		norm, err := NormalizeArticle(raw)
		if err != nil {
			continue
		}
		out = append(out, norm)
	}
	return out
}

func convertAtomEntries(entries []atomEntry, cfg SourceConfig, fetchedAt time.Time, limit int) []UnifiedArticle {
	var out []UnifiedArticle
	for _, entry := range entries {
		if limit > 0 && len(out) >= limit {
			break
		}
		link := pickAtomLink(entry.Links)
		desc := entry.Summary
		if strings.TrimSpace(desc) == "" {
			desc = entry.Content
		}
		rawDate := entry.Published
		if strings.TrimSpace(rawDate) == "" {
			rawDate = entry.Updated
		}
		var cats []string
		for _, c := range entry.Categories {
			if strings.TrimSpace(c.Term) != "" {
				cats = append(cats, c.Term)
			} else if strings.TrimSpace(c.Label) != "" {
				cats = append(cats, c.Label)
			} else if strings.TrimSpace(c.Value) != "" {
				cats = append(cats, c.Value)
			}
		}
		extID := strings.TrimSpace(entry.ID)
		if extID == "" {
			extID = link
		}

		raw := UnifiedArticle{
			SourceID:     cfg.ID,
			ExternalID:   extID,
			Title:        entry.Title,
			Description:  desc,
			OriginalURL:  link,
			SourceType:   SourceTypeAtom,
			Author:       entry.Author.Name,
			PublishedAt:  ParseDate(rawDate),
			DiscoveredAt: fetchedAt,
			FetchedAt:    fetchedAt,
			Language:     cfg.Language,
			Categories:   cats,
			RawMetadata: map[string]any{
				"source_name": cfg.Name,
				"feed_url":    cfg.FeedURL,
			},
		}
		norm, err := NormalizeArticle(raw)
		if err != nil {
			continue
		}
		out = append(out, norm)
	}
	return out
}

func pickAtomLink(links []atomLink) string {
	for _, l := range links {
		if strings.TrimSpace(l.Href) == "" {
			continue
		}
		if l.Rel == "" || strings.EqualFold(l.Rel, "alternate") {
			return l.Href
		}
	}
	for _, l := range links {
		if strings.TrimSpace(l.Href) != "" {
			return l.Href
		}
	}
	return ""
}
