package news

import (
	"fmt"
	"html"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTitleRunes       = 500
	MaxDescriptionRunes = 2000
)

var (
	scriptStyleRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)>`)
	htmlTagRe     = regexp.MustCompile(`<[^>]+>`)
)

var trackingQueryKeys = map[string]struct{}{
	"fbclid":  {},
	"gclid":   {},
	"mc_cid":  {},
	"mc_eid":  {},
	"yclid":   {},
	"igshid":  {},
	"_hsenc":  {},
	"_hsmi":   {},
	"ref":     {},
	"spm":     {},
	"si":      {},
	"feature": {},
}

// NormalizeURL produces a canonical URL by lowercasing scheme/host, stripping
// default ports and fragments, removing known tracking parameters, and sorting
// remaining query parameters deterministically.
func NormalizeURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("news: empty url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("news: invalid url: %w", err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: %q", ErrInvalidScheme, parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("news: url missing host")
	}

	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		// Bare IPv6 address without port.
		parsed.Host = "[" + host + "]"
	} else {
		parsed.Host = host
	}

	parsed.Scheme = scheme
	parsed.Fragment = ""
	parsed.RawFragment = ""

	if parsed.Path == "" {
		parsed.Path = "/"
	}

	if parsed.RawQuery != "" {
		q, err := url.ParseQuery(parsed.RawQuery)
		if err == nil {
			cleaned := url.Values{}
			for k, vals := range q {
				lk := strings.ToLower(strings.TrimSpace(k))
				if strings.HasPrefix(lk, "utm_") {
					continue
				}
				if _, isTracking := trackingQueryKeys[lk]; isTracking {
					continue
				}
				for _, v := range vals {
					cleaned.Add(k, v)
				}
			}
			parsed.RawQuery = encodeSortedQuery(cleaned)
		}
	}

	return parsed.String(), nil
}

func encodeSortedQuery(v url.Values) string {
	if len(v) == 0 {
		return ""
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		vals := append([]string(nil), v[k]...)
		sort.Strings(vals)
		ek := url.QueryEscape(k)
		for _, val := range vals {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(ek)
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(val))
		}
	}
	return b.String()
}

// NormalizeText strips script/style blocks and HTML tags, decodes HTML
// entities, removes control characters, normalizes whitespace, ensures valid
// UTF-8, and truncates to maxRunes.
func NormalizeText(raw string, maxRunes int) string {
	if raw == "" {
		return ""
	}
	s := strings.ToValidUTF8(raw, "")
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	// Strip any tags revealed after entity unescaping (e.g. &lt;script&gt;).
	s = scriptStyleRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")

	var b strings.Builder
	b.Grow(len(s))
	inSpace := true
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		b.WriteRune(r)
		inSpace = false
	}
	out := strings.TrimSpace(b.String())
	if maxRunes > 0 && utf8.RuneCountInString(out) > maxRunes {
		runes := []rune(out)
		out = strings.TrimSpace(string(runes[:maxRunes]))
	}
	return out
}

// NormalizeTitleForDedup lowercases and strips punctuation for Level 3
// near-duplicate title detection.
func NormalizeTitleForDedup(raw string) string {
	clean := NormalizeText(raw, MaxTitleRunes)
	if clean == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(clean))
	inSpace := true
	for _, r := range strings.ToLower(clean) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			inSpace = false
		} else if !inSpace {
			b.WriteByte(' ')
			inSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

// TitleSimilarity computes Jaccard token similarity in [0, 1] between two
// normalized titles.
func TitleSimilarity(a, b string) float64 {
	na := NormalizeTitleForDedup(a)
	nb := NormalizeTitleForDedup(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	tokensA := strings.Fields(na)
	tokensB := strings.Fields(nb)
	setA := make(map[string]struct{}, len(tokensA))
	for _, t := range tokensA {
		setA[t] = struct{}{}
	}
	setB := make(map[string]struct{}, len(tokensB))
	for _, t := range tokensB {
		setB[t] = struct{}{}
	}
	inter := 0
	for t := range setA {
		if _, ok := setB[t]; ok {
			inter++
		}
	}
	union := len(setA) + len(setB) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 2006 15:04:05 -0700 (MST)",
	"2 Jan 2006 15:04:05 -0700",
	"2006-01-02T15:04:05-07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// ParseDate parses RFC3339, RFC822/RFC1123, common RSS date formats, and Unix
// timestamps into UTC. Returns nil if the input is empty or unparseable.
func ParseDate(raw string) *time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n <= 0 {
			return nil
		}
		// Treat values >= 1e12 as milliseconds.
		var t time.Time
		if n >= 1_000_000_000_000 {
			t = time.UnixMilli(n).UTC()
		} else {
			t = time.Unix(n, 0).UTC()
		}
		return &t
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			utc := t.UTC()
			return &utc
		}
	}
	return nil
}

// NormalizeArticle normalizes URL, text fields, timestamps, and deterministic ID
// for a raw UnifiedArticle.
func NormalizeArticle(a UnifiedArticle) (UnifiedArticle, error) {
	a.Title = NormalizeText(a.Title, MaxTitleRunes)
	if a.Title == "" {
		return UnifiedArticle{}, fmt.Errorf("news: article missing title")
	}
	a.Description = NormalizeText(a.Description, MaxDescriptionRunes)
	a.Author = NormalizeText(a.Author, 200)
	a.NormalizedTitle = NormalizeTitleForDedup(a.Title)

	a.OriginalURL = strings.TrimSpace(a.OriginalURL)
	if a.OriginalURL == "" {
		return UnifiedArticle{}, fmt.Errorf("news: article missing url")
	}
	canonical, err := NormalizeURL(a.OriginalURL)
	if err != nil {
		return UnifiedArticle{}, err
	}
	a.CanonicalURL = canonical

	if a.PublishedAt != nil {
		utc := a.PublishedAt.UTC()
		a.PublishedAt = &utc
	}
	if a.SourceSubmittedAt != nil {
		utc := a.SourceSubmittedAt.UTC()
		a.SourceSubmittedAt = &utc
	}
	if a.FetchedAt.IsZero() {
		a.FetchedAt = time.Now().UTC()
	} else {
		a.FetchedAt = a.FetchedAt.UTC()
	}
	if a.DiscoveredAt.IsZero() {
		a.DiscoveredAt = a.FetchedAt
	} else {
		a.DiscoveredAt = a.DiscoveredAt.UTC()
	}

	if a.ID == "" {
		a.ID = DeterministicArticleID(a.SourceID, a.ExternalID, a.CanonicalURL)
	}
	if len(a.Categories) > 0 {
		seen := make(map[string]struct{}, len(a.Categories))
		cleanCats := make([]string, 0, len(a.Categories))
		for _, c := range a.Categories {
			nc := NormalizeText(c, 100)
			if nc == "" {
				continue
			}
			key := strings.ToLower(nc)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			cleanCats = append(cleanCats, nc)
		}
		a.Categories = cleanCats
	}
	if a.RawMetadata == nil {
		a.RawMetadata = map[string]any{}
	}
	return a, nil
}
