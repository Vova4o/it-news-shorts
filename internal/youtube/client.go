package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// API endpoints. Tests override them with WithBaseURL and WithUploadURL.
const (
	defaultBaseURL   = "https://www.googleapis.com/youtube/v3"
	defaultUploadURL = "https://www.googleapis.com/upload/youtube/v3"
)

// Client talks to the YouTube Data API v3. It is a thin transport adapter: the
// domain never imports it, and it depends on no Google SDK beyond OAuth.
type Client struct {
	http           *http.Client
	baseURL        string
	uploadURL      string
	journal        *Journal
	maxAttempts    int
	baseDelay      time.Duration
	maxDelay       time.Duration
	requestTimeout time.Duration
	chunkSize      int64
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error

	mu  sync.Mutex
	rnd *rand.Rand
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient injects the transport, for example one without OAuth in tests.
func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) { c.http = client }
}

// WithBaseURL overrides the Data API endpoint.
func WithBaseURL(raw string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(raw, "/") }
}

// WithUploadURL overrides the resumable upload endpoint.
func WithUploadURL(raw string) Option {
	return func(c *Client) { c.uploadURL = strings.TrimRight(raw, "/") }
}

// WithJournal attaches the idempotency journal.
func WithJournal(journal *Journal) Option {
	return func(c *Client) { c.journal = journal }
}

// WithRetryPolicy sets the retry budget and the backoff bounds.
func WithRetryPolicy(maxAttempts int, base, max time.Duration) Option {
	return func(c *Client) {
		if maxAttempts > 0 {
			c.maxAttempts = maxAttempts
		}
		if base > 0 {
			c.baseDelay = base
		}
		if max > 0 {
			c.maxDelay = max
		}
	}
}

// WithTimeout sets the per-request timeout.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.requestTimeout = timeout
		}
	}
}

// WithChunkSize sets the resumable chunk size; it must be a positive multiple of 256 KiB.
func WithChunkSize(size int64) Option {
	return func(c *Client) {
		c.chunkSize = size
	}
}

// WithClock replaces the clock and the sleep function, so tests run instantly.
func WithClock(now func() time.Time, sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if now != nil {
			c.now = now
		}
		if sleep != nil {
			c.sleep = sleep
		}
	}
}

// NewClient builds a client. A nil tokenSource is allowed for tests that use
// WithHTTPClient.
func NewClient(tokenSource oauth2.TokenSource, opts ...Option) *Client {
	client := &Client{
		baseURL:        defaultBaseURL,
		uploadURL:      defaultUploadURL,
		maxAttempts:    5,
		baseDelay:      500 * time.Millisecond,
		maxDelay:       30 * time.Second,
		requestTimeout: 120 * time.Second,
		chunkSize:      8 << 20,
		now:            time.Now,
		sleep:          sleepContext,
		rnd:            rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	if tokenSource != nil {
		client.http = &http.Client{
			Transport: &oauth2.Transport{Source: tokenSource, Base: http.DefaultTransport},
		}
	} else {
		client.http = &http.Client{Timeout: 120 * time.Second}
	}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

// HTTPClient exposes the transport, so the CLI can build a channel checker.
func (c *Client) HTTPClient() *http.Client { return c.http }

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---- API resource types -------------------------------------------------

type videoResource struct {
	ID      string `json:"id"`
	Snippet struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		CategoryID  string `json:"categoryId"`
	} `json:"snippet"`
	Status struct {
		UploadStatus            string `json:"uploadStatus"`
		PrivacyStatus           string `json:"privacyStatus"`
		PublishAt               string `json:"publishAt"`
		License                 string `json:"license"`
		Embeddable              *bool  `json:"embeddable"`
		PublicStatsViewable     *bool  `json:"publicStatsViewable"`
		SelfDeclaredMadeForKids *bool  `json:"selfDeclaredMadeForKids"`
		RejectionReason         string `json:"rejectionReason"`
		FailureReason           string `json:"failureReason"`
	} `json:"status"`
	ProcessingDetails struct {
		ProcessingStatus        string `json:"processingStatus"`
		ProcessingFailureReason string `json:"processingFailureReason"`
	} `json:"processingDetails"`
}

type listResponse struct {
	Items []videoResource `json:"items"`
}

type channelResponse struct {
	Items []struct {
		ID      string `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
	} `json:"items"`
}

type apiErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
}

// APIError is a non-2xx answer from the Data API.
type APIError struct {
	StatusCode int
	Message    string
	Reason     string
}

func (e *APIError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("youtube api: status %d (%s): %s", e.StatusCode, e.Reason, e.Message)
	}
	return fmt.Sprintf("youtube api: status %d: %s", e.StatusCode, e.Message)
}

func parseAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &APIError{StatusCode: resp.StatusCode}
	var parsed apiErrorBody
	if err := json.Unmarshal(body, &parsed); err == nil {
		apiErr.Message = parsed.Error.Message
		if len(parsed.Error.Errors) > 0 {
			apiErr.Reason = parsed.Error.Errors[0].Reason
		}
	}
	if apiErr.Message == "" {
		rawText := strings.TrimSpace(string(body))
		if len(rawText) > 200 {
			rawText = rawText[:200] + "..."
		}
		if strings.Contains(rawText, "access_token") || strings.Contains(rawText, "refresh_token") || strings.Contains(rawText, "client_secret") {
			rawText = "[redacted response body]"
		}
		apiErr.Message = rawText
	}
	if apiErr.Message == "" && readErr != nil {
		apiErr.Message = fmt.Sprintf("read error body: %v", readErr)
	}
	if resp.StatusCode == http.StatusForbidden && isPermissionReason(apiErr.Reason) {
		return fmt.Errorf("%w: %w", ErrInsufficientScope, apiErr)
	}
	return apiErr
}

func isPermissionReason(reason string) bool {
	switch reason {
	case "insufficientPermissions", "insufficientScopes", "youtubeSignupRequired", "forbidden", "accessNotConfigured":
		return true
	default:
		return false
	}
}

// ---- request plumbing ---------------------------------------------------

func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.requestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

func (c *Client) newRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	return req, nil
}

func (c *Client) doHTTP(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, sanitizeTransportError(err)
	}
	return resp, nil
}

func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &url.Error{
			Op:  urlErr.Op,
			URL: redactURLString(urlErr.URL),
			Err: sanitizeTransportError(urlErr.Err),
		}
	}
	return err
}

func redactURLString(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		if idx := strings.IndexByte(raw, '?'); idx >= 0 {
			return raw[:idx] + "?redacted"
		}
		return raw
	}
	hadQuery := parsed.RawQuery != "" || parsed.ForceQuery
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	if parsed.Path != "" {
		parts := strings.Split(parsed.Path, "/")
		for i, seg := range parts {
			if len(seg) > 24 {
				parts[i] = "<redacted>"
			}
		}
		parsed.Path = strings.Join(parts, "/")
	}
	if hadQuery {
		return parsed.String() + "?redacted"
	}
	return parsed.String()
}

// doRetry performs a request and retries transient failures with exponential
// backoff and jitter, honouring Retry-After.
func (c *Client) doRetry(ctx context.Context, build func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := c.doHTTP(req)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			lastErr = err
			if attempt == c.maxAttempts-1 {
				break
			}
			if err := c.sleep(ctx, c.backoff(attempt, 0)); err != nil {
				return nil, err
			}
			continue
		}
		if !retryableStatus(resp.StatusCode) {
			return resp, nil
		}
		lastErr = fmt.Errorf("youtube api: status %d", resp.StatusCode)
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
		drainAndClose(resp.Body)
		if attempt == c.maxAttempts-1 {
			break
		}
		if err := c.sleep(ctx, c.backoff(attempt, retryAfter)); err != nil {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("unknown transport failure")
	}
	return nil, fmt.Errorf("%w: %w", ErrRetriesExhausted, lastErr)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// backoff returns the delay before the next attempt: exponential growth with
// full jitter, bounded by maxDelay. A server-provided Retry-After is a minimum
// wait and is never shortened below what the server asked for.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	delay := c.baseDelay
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= c.maxDelay {
			delay = c.maxDelay
			break
		}
	}
	c.mu.Lock()
	jitter := time.Duration(0)
	if c.rnd != nil && delay > 0 {
		jitter = time.Duration(c.rnd.Int63n(int64(delay/2) + 1))
	}
	c.mu.Unlock()
	return delay/2 + jitter
}

// parseRetryAfter accepts both delay-seconds and an HTTP date.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := when.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	body.Close()
}

func decodeJSON(resp *http.Response, target any) error {
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("youtube: read response: %w", err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("youtube: decode response: %w", err)
	}
	return nil
}

// ---- public API ---------------------------------------------------------

// GetChannel returns the channel that owns the credentials.
func (c *Client) GetChannel(ctx context.Context) (Channel, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	rawURL := c.baseURL + "/channels?part=snippet&mine=true"
	resp, err := c.doRetry(ctx, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, rawURL, nil)
	})
	if err != nil {
		return Channel{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Channel{}, parseAPIError(resp)
	}
	var parsed channelResponse
	if err := decodeJSON(resp, &parsed); err != nil {
		return Channel{}, err
	}
	if len(parsed.Items) == 0 {
		return Channel{}, fmt.Errorf("youtube: credentials are not attached to a channel")
	}
	return Channel{ID: parsed.Items[0].ID, Title: parsed.Items[0].Snippet.Title}, nil
}

// VerifyChannel returns the authorized channel and ensures it matches expectedID
// when expectedID is non-empty.
func (c *Client) VerifyChannel(ctx context.Context, expectedID string) (Channel, error) {
	channel, err := c.GetChannel(ctx)
	if err != nil {
		return Channel{}, err
	}
	expectedID = strings.TrimSpace(expectedID)
	if expectedID != "" && channel.ID != expectedID {
		return channel, fmt.Errorf("%w: got %s, want %s", ErrChannelMismatch, channel.ID, expectedID)
	}
	return channel, nil
}

// GetVideo reads one video with the parts needed for status reporting.
func (c *Client) GetVideo(ctx context.Context, videoID string) (VideoStatus, error) {
	resource, err := c.getVideoResource(ctx, videoID)
	if err != nil {
		return VideoStatus{}, err
	}
	return toVideoStatus(resource), nil
}

// GetProcessingStatus returns only the processing status of a video.
func (c *Client) GetProcessingStatus(ctx context.Context, videoID string) (VideoStatus, error) {
	return c.GetVideo(ctx, videoID)
}

func (c *Client) getVideoResource(ctx context.Context, videoID string) (videoResource, error) {
	if strings.TrimSpace(videoID) == "" {
		return videoResource{}, fmt.Errorf("youtube: video id is required")
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	rawURL := c.baseURL + "/videos?part=snippet,status,processingDetails&id=" + url.QueryEscape(videoID)
	resp, err := c.doRetry(ctx, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, rawURL, nil)
	})
	if err != nil {
		return videoResource{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return videoResource{}, parseAPIError(resp)
	}
	var parsed listResponse
	if err := decodeJSON(resp, &parsed); err != nil {
		return videoResource{}, err
	}
	if len(parsed.Items) == 0 {
		return videoResource{}, fmt.Errorf("%w: %s", ErrVideoNotFound, videoID)
	}
	return parsed.Items[0], nil
}

func toVideoStatus(resource videoResource) VideoStatus {
	status := VideoStatus{
		VideoID:          resource.ID,
		Title:            resource.Snippet.Title,
		PrivacyStatus:    resource.Status.PrivacyStatus,
		UploadStatus:     resource.Status.UploadStatus,
		ProcessingStatus: resource.ProcessingDetails.ProcessingStatus,
		FailureReason:    firstNonEmpty(resource.ProcessingDetails.ProcessingFailureReason, resource.Status.FailureReason),
		RejectionReason:  resource.Status.RejectionReason,
	}
	if resource.Status.PublishAt != "" {
		if parsed, err := time.Parse(time.RFC3339, resource.Status.PublishAt); err == nil {
			status.PublishAt = parsed.UTC()
		}
	}
	return status
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (c *Client) ensurePublishAtFuture(publishAt time.Time) (time.Time, error) {
	formatted := publishAt.UTC().Truncate(time.Second)
	if !formatted.After(c.now().UTC()) {
		return time.Time{}, fmt.Errorf("%w: %s", ErrPublishAtNotFuture, formatted.Format(time.RFC3339))
	}
	return formatted, nil
}

// SchedulePublication sets a future publish time on a private video.
//
// Note on API limits: videos.list exposes only the current privacyStatus and
// not full historical visibility transitions, so checking privacyStatus == "private"
// is the strongest guard the YouTube Data API supports for ensuring a video is
// not currently published.
//
// With dryRun the plan is returned and no changing request is sent. The update
// uses part=status and echoes the current license, embeddable,
// publicStatsViewable and selfDeclaredMadeForKids values, so unrelated
// properties are not silently reset.
func (c *Client) SchedulePublication(ctx context.Context, videoID string, publishAt time.Time, dryRun bool) (SchedulePlan, error) {
	if strings.TrimSpace(videoID) == "" {
		return SchedulePlan{}, fmt.Errorf("youtube: video id is required")
	}
	var err error
	publishAt, err = c.ensurePublishAtFuture(publishAt)
	if err != nil {
		return SchedulePlan{}, err
	}

	current, err := c.getVideoResource(ctx, videoID)
	if err != nil {
		return SchedulePlan{}, err
	}
	if current.Status.PrivacyStatus == string(PrivacyPublic) {
		return SchedulePlan{}, fmt.Errorf("%w: %s is %s", ErrAlreadyPublished, videoID, current.Status.PrivacyStatus)
	}
	if current.Status.PrivacyStatus != string(PrivacyPrivate) {
		return SchedulePlan{}, fmt.Errorf("%w: %s is %s", ErrVideoNotPrivate, videoID, current.Status.PrivacyStatus)
	}
	if current.Status.UploadStatus == "rejected" || current.Status.UploadStatus == "failed" {
		return SchedulePlan{}, fmt.Errorf("youtube: cannot schedule %s with upload status %s", videoID, current.Status.UploadStatus)
	}

	// Re-check after videos.list so a slow GET cannot let an elapsed publishAt
	// pass into a dry-run plan or a mutating PUT (YouTube publishes past times immediately).
	publishAt, err = c.ensurePublishAtFuture(publishAt)
	if err != nil {
		return SchedulePlan{}, err
	}

	update := map[string]any{
		"id": videoID,
		"status": map[string]any{
			"privacyStatus": string(PrivacyPrivate),
			"publishAt":     publishAt.Format(time.RFC3339),
		},
	}
	status := update["status"].(map[string]any)
	if current.Status.License != "" {
		status["license"] = current.Status.License
	}
	if current.Status.Embeddable != nil {
		status["embeddable"] = *current.Status.Embeddable
	}
	if current.Status.PublicStatsViewable != nil {
		status["publicStatsViewable"] = *current.Status.PublicStatsViewable
	}
	if current.Status.SelfDeclaredMadeForKids != nil {
		status["selfDeclaredMadeForKids"] = *current.Status.SelfDeclaredMadeForKids
	}
	body, err := json.Marshal(update)
	if err != nil {
		return SchedulePlan{}, fmt.Errorf("youtube: encode schedule request: %w", err)
	}

	plan := SchedulePlan{
		VideoID:   videoID,
		PublishAt: publishAt,
		Privacy:   PrivacyPrivate,
		DryRun:    dryRun,
		Request:   string(body),
	}
	if dryRun {
		return plan, nil
	}

	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	rawURL := c.baseURL + "/videos?part=status"
	resp, err := c.doRetry(reqCtx, func() (*http.Request, error) {
		// Re-check immediately before each PUT attempt (including retries).
		if _, err := c.ensurePublishAtFuture(publishAt); err != nil {
			return nil, err
		}
		return c.newRequest(reqCtx, http.MethodPut, rawURL, body)
	})
	if err != nil {
		return SchedulePlan{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return SchedulePlan{}, parseAPIError(resp)
	}
	drainAndClose(resp.Body)
	return plan, nil
}
