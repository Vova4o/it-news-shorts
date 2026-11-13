package news

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Typed HTTP client errors for retry classification and security assertions.
var (
	ErrInvalidScheme     = errors.New("news: unsupported URL scheme")
	ErrSSRFBlocked       = errors.New("news: request blocked by SSRF policy")
	ErrOversizedResponse = errors.New("news: response exceeded maximum allowed size")
	ErrTooManyRedirects  = errors.New("news: too many redirects")
)

// UpstreamError represents an unexpected non-2xx/304 HTTP status code.
type UpstreamError struct {
	StatusCode int
	RetryAfter time.Duration
	URL        string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("news: upstream returned status %d for %s", e.StatusCode, e.URL)
}

// Retryable reports whether the upstream HTTP status warrants a retry.
func (e *UpstreamError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// HTTPClientOptions configures the hardened news HTTP client.
type HTTPClientOptions struct {
	Timeout         time.Duration
	MaxBytes        int64
	MaxConcurrency  int
	MaxRedirects    int
	MaxRetries      int
	BaseBackoff     time.Duration
	MaxBackoff      time.Duration
	UserAgent       string
	AllowPrivateIPs bool // Only for unit tests using httptest.Server on loopback.
	Transport       http.RoundTripper
}

// HTTPResponse holds the fetched payload and caching headers.
type HTTPResponse struct {
	StatusCode   int
	Body         []byte
	ETag         string
	LastModified string
	NotModified  bool
	FinalURL     string
}

// HTTPClient performs SSRF-protected, rate-limited, retryable HTTP GET requests.
type HTTPClient struct {
	client       *http.Client
	maxBytes     int64
	maxRedirects int
	maxRetries   int
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	userAgent    string
	allowPrivate bool
	sem          chan struct{}
}

// NewHTTPClient creates a hardened HTTPClient with SSRF protection, bounded
// concurrency, response size limits, and exponential backoff retries.
func NewHTTPClient(opts HTTPClientOptions) *HTTPClient {
	if opts.Timeout <= 0 {
		opts.Timeout = 15 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20
	}
	if opts.MaxConcurrency <= 0 {
		opts.MaxConcurrency = 5
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = 5
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	} else if opts.MaxRetries == 0 {
		opts.MaxRetries = 3
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = 500 * time.Millisecond
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 15 * time.Second
	}
	if strings.TrimSpace(opts.UserAgent) == "" {
		opts.UserAgent = "it-news-shorts/1.0"
	}

	c := &HTTPClient{
		maxBytes:     opts.MaxBytes,
		maxRedirects: opts.MaxRedirects,
		maxRetries:   opts.MaxRetries,
		baseBackoff:  opts.BaseBackoff,
		maxBackoff:   opts.MaxBackoff,
		userAgent:    opts.UserAgent,
		allowPrivate: opts.AllowPrivateIPs,
		sem:          make(chan struct{}, opts.MaxConcurrency),
	}

	transport := opts.Transport
	if transport == nil {
		dialer := &net.Dialer{
			Timeout:   opts.Timeout,
			KeepAlive: 30 * time.Second,
			Control: func(network, address string, _ syscall.RawConn) error {
				if c.allowPrivate {
					return nil
				}
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return fmt.Errorf("%w: invalid dial address", ErrSSRFBlocked)
				}
				ip := net.ParseIP(host)
				if ip == nil || isDisallowedIP(ip) {
					return fmt.Errorf("%w: ip %s is disallowed", ErrSSRFBlocked, host)
				}
				return nil
			},
		}
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}

	c.client = &http.Client{
		Timeout:   opts.Timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > c.maxRedirects {
				return fmt.Errorf("%w: exceeded %d redirects", ErrTooManyRedirects, c.maxRedirects)
			}
			return c.validateURL(req.Context(), req.URL)
		},
	}

	return c
}

// Get fetches rawURL with optional conditional-GET headers and retries.
func (c *HTTPClient) Get(ctx context.Context, rawURL, etag, lastModified string) (HTTPResponse, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("news: parse url: %w", err)
	}
	if err := c.validateURL(ctx, parsed); err != nil {
		return HTTPResponse{}, err
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return HTTPResponse{}, ctx.Err()
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return HTTPResponse{}, err
		}

		resp, err := c.doOnce(ctx, parsed.String(), etag, lastModified)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !isRetryableError(err) || attempt == c.maxRetries {
			return HTTPResponse{}, err
		}

		wait := c.backoffDuration(attempt, err)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return HTTPResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
	return HTTPResponse{}, lastErr
}

func (c *HTTPClient) doOnce(ctx context.Context, targetURL, etag, lastModified string) (HTTPResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("news: create request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml, application/json, text/xml;q=0.9, */*;q=0.8")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return HTTPResponse{}, err
	}
	defer resp.Body.Close()

	finalURL := targetURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	if resp.StatusCode == http.StatusNotModified {
		return HTTPResponse{
			StatusCode:   resp.StatusCode,
			NotModified:  true,
			ETag:         resp.Header.Get("ETag"),
			LastModified: resp.Header.Get("Last-Modified"),
			FinalURL:     finalURL,
		}, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return HTTPResponse{}, &UpstreamError{
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			URL:        finalURL,
		}
	}

	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return HTTPResponse{}, fmt.Errorf("news: read response body: %w", err)
	}
	if int64(len(body)) > c.maxBytes {
		return HTTPResponse{}, fmt.Errorf("%w: limit is %d bytes", ErrOversizedResponse, c.maxBytes)
	}

	return HTTPResponse{
		StatusCode:   resp.StatusCode,
		Body:         body,
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FinalURL:     finalURL,
	}, nil
}

func (c *HTTPClient) validateURL(ctx context.Context, u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: empty url", ErrInvalidScheme)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("%w: %q", ErrInvalidScheme, u.Scheme)
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrSSRFBlocked)
	}
	if c.allowPrivate {
		return nil
	}

	lowerHost := strings.ToLower(host)
	if lowerHost == "localhost" || strings.HasSuffix(lowerHost, ".localhost") || strings.HasSuffix(lowerHost, ".local") || strings.HasSuffix(lowerHost, ".internal") {
		return fmt.Errorf("%w: host %q is disallowed", ErrSSRFBlocked, host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isDisallowedIP(ip) {
			return fmt.Errorf("%w: ip %s is disallowed", ErrSSRFBlocked, ip.String())
		}
		return nil
	}

	// Resolve DNS eagerly when possible so non-dial transports or redirects
	// cannot target private IPs.
	if ips, err := net.DefaultResolver.LookupIPAddr(ctx, host); err == nil {
		for _, ipAddr := range ips {
			if isDisallowedIP(ipAddr.IP) {
				return fmt.Errorf("%w: host %q resolved to disallowed ip %s", ErrSSRFBlocked, host, ipAddr.IP.String())
			}
		}
	}
	return nil
}

func isDisallowedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrInvalidScheme) ||
		errors.Is(err, ErrSSRFBlocked) ||
		errors.Is(err, ErrOversizedResponse) ||
		errors.Is(err, ErrTooManyRedirects) {
		return false
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Retryable()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if errors.Is(urlErr.Err, ErrSSRFBlocked) ||
			errors.Is(urlErr.Err, ErrInvalidScheme) ||
			errors.Is(urlErr.Err, ErrTooManyRedirects) {
			return false
		}
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func (c *HTTPClient) backoffDuration(attempt int, err error) time.Duration {
	var upstream *UpstreamError
	if errors.As(err, &upstream) && upstream.RetryAfter > 0 {
		if upstream.RetryAfter > c.maxBackoff {
			return c.maxBackoff
		}
		return upstream.RetryAfter
	}
	backoff := c.baseBackoff * (1 << attempt)
	if backoff > c.maxBackoff {
		backoff = c.maxBackoff
	}
	// Add up to 25% jitter.
	jitter := time.Duration(rand.Int63n(int64(backoff/4) + 1))
	return backoff + jitter
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := t.Sub(now)
		if d > 0 {
			return d
		}
	}
	return 0
}
