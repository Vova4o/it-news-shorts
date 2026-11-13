package youtube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const minChunkSize = 256 * 1024

// UploadVideo uploads a local file through a resumable session.
//
// The operation requires a journal and acquires an exclusive per-key lock: a
// repeated call with the same idempotency key returns the recorded video instead
// of uploading again (even if the local source file has since been removed),
// and an ambiguous outcome is recorded as UNKNOWN rather than retried blindly.
func (c *Client) UploadVideo(ctx context.Context, in UploadInput) (UploadResult, error) {
	in = in.Normalize()
	if err := in.Validate(); err != nil {
		return UploadResult{}, err
	}
	if c.journal == nil {
		return UploadResult{}, errors.New("youtube: upload journal is required")
	}
	if c.chunkSize <= 0 || c.chunkSize%minChunkSize != 0 {
		return UploadResult{}, fmt.Errorf("youtube: chunk size %d must be a positive multiple of %d bytes", c.chunkSize, minChunkSize)
	}

	release, err := c.journal.AcquireKey(ctx, in.IdempotencyKey)
	if err != nil {
		return UploadResult{}, err
	}
	defer release()

	entry, found, err := c.journal.Get(ctx, in.IdempotencyKey)
	if err != nil {
		return UploadResult{}, err
	}
	if found && entry.State == UploadStateCompleted {
		return c.replayCompleted(ctx, in, entry)
	}

	file, total, fileHash, err := openRegularAndHash(ctx, in.FilePath)
	if err != nil {
		return UploadResult{}, err
	}
	defer file.Close()

	if found {
		if err := verifyFileMatchesEntry(entry, in.IdempotencyKey, fileHash, total); err != nil {
			return UploadResult{}, err
		}
		switch entry.State {
		case UploadStateFailed:
			return UploadResult{State: UploadStateFailed}, fmt.Errorf(
				"%w: key %s previously failed (%s) and will not be re-uploaded automatically",
				ErrUploadUnknown, in.IdempotencyKey, entry.Error)
		case UploadStatePending:
			if strings.TrimSpace(entry.SessionURI) == "" {
				return c.recordUnknown(ctx, in, "", total, entry.SentBytes, fileHash,
					errors.New("previous upload crashed before session creation completed"))
			}
		case UploadStateInProgress:
			if strings.TrimSpace(entry.SessionURI) == "" {
				return c.recordUnknown(ctx, in, "", total, entry.SentBytes, fileHash,
					errors.New("in-progress upload record is missing session_uri"))
			}
		case UploadStateUnknown:
			if entry.SessionURI != "" {
				outcome, queryErr := c.querySession(ctx, entry.SessionURI, total)
				if queryErr == nil && outcome.completed {
					return c.completeUpload(ctx, in, entry.SessionURI, outcome.resource, fileHash, total, true)
				}
			}
			return UploadResult{State: UploadStateUnknown}, fmt.Errorf(
				"%w: key %s is recorded as %s and must be reviewed before another upload",
				ErrUploadUnknown, in.IdempotencyKey, entry.State)
		}
	}

	sessionURI := ""
	if found && entry.SessionURI != "" &&
		(entry.State == UploadStateInProgress || entry.State == UploadStatePending) {
		sessionURI = entry.SessionURI
	}

	resumed := false
	offset := int64(0)
	if sessionURI != "" {
		outcome, queryErr := c.querySession(ctx, sessionURI, total)
		switch {
		case queryErr == nil && outcome.completed:
			return c.completeUpload(ctx, in, sessionURI, outcome.resource, fileHash, total, true)
		case queryErr == nil:
			if outcome.offset < 0 || outcome.offset > total {
				return c.recordUnknown(ctx, in, sessionURI, total, entry.SentBytes, fileHash,
					fmt.Errorf("server returned invalid offset %d for total %d", outcome.offset, total))
			}
			offset = outcome.offset
			resumed = true
			if outcome.retryAfter > 0 {
				if err := c.sleep(ctx, c.backoff(0, outcome.retryAfter)); err != nil {
					return c.recordUnknown(ctx, in, sessionURI, total, offset, fileHash, err)
				}
			}
		default:
			return c.recordUnknown(ctx, in, sessionURI, total, entry.SentBytes, fileHash, queryErr)
		}
	}

	if sessionURI == "" {
		if err := c.journal.Put(ctx, JournalEntry{
			Key: in.IdempotencyKey, State: UploadStatePending,
			TotalBytes: total, SentBytes: 0, FileSHA256: fileHash,
			Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
		}); err != nil {
			return UploadResult{}, err
		}
		sessionURI, err = c.createSession(ctx, in, total)
		if err != nil {
			recordErr := c.journal.Put(ctx, JournalEntry{
				Key: in.IdempotencyKey, State: UploadStateFailed,
				TotalBytes: total, SentBytes: 0, FileSHA256: fileHash,
				Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
				Error: err.Error(),
			})
			if recordErr != nil {
				return UploadResult{State: UploadStateFailed}, fmt.Errorf("%w (journal update failed: %v)", err, recordErr)
			}
			return UploadResult{State: UploadStateFailed}, err
		}
		offset = 0
	}

	if err := c.journal.Put(ctx, JournalEntry{
		Key: in.IdempotencyKey, State: UploadStateInProgress, SessionURI: sessionURI,
		TotalBytes: total, SentBytes: offset, FileSHA256: fileHash,
		Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
	}); err != nil {
		return UploadResult{}, err
	}

	resource, lastOffset, err := c.uploadChunks(ctx, file, sessionURI, offset, total, in, fileHash)
	if err == nil {
		return c.completeUpload(ctx, in, sessionURI, resource, fileHash, total, resumed || offset > 0)
	}

	if errors.Is(err, ErrSessionLost) {
		return c.recordUnknown(ctx, in, sessionURI, total, lastOffset, fileHash, err)
	}

	// The transfer failed. Before reporting anything, ask the session whether
	// the bytes actually landed; a lost response must not become a duplicate.
	outcome, queryErr := c.querySession(ctx, sessionURI, total)
	if queryErr == nil && outcome.completed {
		return c.completeUpload(ctx, in, sessionURI, outcome.resource, fileHash, total, true)
	}
	if queryErr == nil && outcome.offset > lastOffset && outcome.offset <= total {
		lastOffset = outcome.offset
	}
	return c.recordUnknown(ctx, in, sessionURI, total, lastOffset, fileHash, err)
}

func (c *Client) replayCompleted(ctx context.Context, in UploadInput, entry JournalEntry) (UploadResult, error) {
	file, total, fileHash, err := openRegularAndHash(ctx, in.FilePath)
	if err == nil {
		file.Close()
		if err := verifyFileMatchesEntry(entry, in.IdempotencyKey, fileHash, total); err != nil {
			return UploadResult{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return UploadResult{}, err
	}

	uploadedAt := entry.UploadedAt
	if uploadedAt.IsZero() {
		uploadedAt = entry.UpdatedAt
	}
	return UploadResult{
		VideoID:       entry.VideoID,
		URL:           VideoURL(entry.VideoID),
		UploadStatus:  entry.UploadStatus,
		PrivacyStatus: entry.Privacy,
		UploadedAt:    uploadedAt,
		State:         UploadStateCompleted,
		Resumed:       true,
	}, nil
}

func (c *Client) recordUnknown(ctx context.Context, in UploadInput, sessionURI string, total, sent int64, fileHash string, cause error) (UploadResult, error) {
	recordErr := c.journal.Put(ctx, JournalEntry{
		Key: in.IdempotencyKey, State: UploadStateUnknown, SessionURI: sessionURI,
		TotalBytes: total, SentBytes: sent, FileSHA256: fileHash,
		Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
		Error: cause.Error(),
	})
	if recordErr != nil {
		return UploadResult{State: UploadStateUnknown}, fmt.Errorf("%w: %w (journal update failed: %v)", ErrUploadUnknown, cause, recordErr)
	}
	return UploadResult{State: UploadStateUnknown}, fmt.Errorf("%w: %w", ErrUploadUnknown, cause)
}

func (c *Client) completeUpload(ctx context.Context, in UploadInput, sessionURI string, resource videoResource, fileHash string, total int64, resumed bool) (UploadResult, error) {
	if resource.ID == "" {
		return c.recordUnknown(ctx, in, sessionURI, total, total, fileHash, errors.New("completed session returned no video id"))
	}
	now := c.now().UTC()
	privacy := resource.Status.PrivacyStatus
	if privacy == "" {
		privacy = string(in.Privacy)
	}
	pubID := ""
	if in.VideoJobID != "" {
		pubID = "pub-" + resource.ID
	}
	if err := c.journal.Put(ctx, JournalEntry{
		Key:           in.IdempotencyKey,
		State:         UploadStateCompleted,
		VideoID:       resource.ID,
		UploadStatus:  resource.Status.UploadStatus,
		TotalBytes:    total,
		SentBytes:     total,
		FileSHA256:    fileHash,
		Title:         in.Title,
		Privacy:       privacy,
		VideoJobID:    in.VideoJobID,
		PublicationID: pubID,
		UploadedAt:    now,
	}); err != nil {
		return UploadResult{}, err
	}
	return UploadResult{
		VideoID:       resource.ID,
		URL:           VideoURL(resource.ID),
		UploadStatus:  resource.Status.UploadStatus,
		PrivacyStatus: privacy,
		UploadedAt:    now,
		State:         UploadStateCompleted,
		Resumed:       resumed,
	}, nil
}

func verifyFileMatchesEntry(entry JournalEntry, key, fileHash string, total int64) error {
	if entry.FileSHA256 != "" && entry.FileSHA256 != fileHash {
		return fmt.Errorf("youtube: idempotency key %s was already used for a different file", key)
	}
	if entry.TotalBytes > 0 && entry.TotalBytes != total {
		return fmt.Errorf("youtube: idempotency key %s was already used for a file of size %d (got %d)", key, entry.TotalBytes, total)
	}
	return nil
}

func openRegularAndHash(ctx context.Context, path string) (*os.File, int64, string, error) {
	preInfo, err := os.Stat(path)
	if err != nil {
		return nil, 0, "", fmt.Errorf("youtube: stat upload file: %w", err)
	}
	if !preInfo.Mode().IsRegular() {
		return nil, 0, "", fmt.Errorf("youtube: upload path %s is not a regular file", path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, "", fmt.Errorf("youtube: open upload file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, 0, "", fmt.Errorf("youtube: stat upload file: %w", err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, 0, "", fmt.Errorf("youtube: upload path %s is not a regular file", path)
	}
	total := info.Size()
	if total <= 0 {
		file.Close()
		return nil, 0, "", fmt.Errorf("youtube: upload file %s is empty", path)
	}
	fileHash, err := sha256File(ctx, file)
	if err != nil {
		file.Close()
		return nil, 0, "", fmt.Errorf("youtube: hash upload file: %w", err)
	}
	return file, total, fileHash, nil
}

func sha256File(ctx context.Context, file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// createSession opens a resumable session and returns its URI.
func (c *Client) createSession(ctx context.Context, in UploadInput, total int64) (string, error) {
	snippet := map[string]any{
		"title":       in.Title,
		"description": in.Description,
		"categoryId":  in.CategoryID,
	}
	if len(in.Tags) > 0 {
		snippet["tags"] = in.Tags
	}
	if in.Language != "" {
		snippet["defaultLanguage"] = in.Language
	}
	status := map[string]any{
		"privacyStatus":           string(in.Privacy),
		"selfDeclaredMadeForKids": in.MadeForKids,
	}
	if in.SyntheticMedia {
		status["containsSyntheticMedia"] = true
	}
	body, err := json.Marshal(map[string]any{"snippet": snippet, "status": status})
	if err != nil {
		return "", fmt.Errorf("youtube: encode upload metadata: %w", err)
	}

	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	rawURL := c.uploadURL + "/videos?uploadType=resumable&part=snippet,status"
	build := func() (*http.Request, error) {
		req, err := c.newRequest(reqCtx, http.MethodPost, rawURL, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(total, 10))
		req.Header.Set("X-Upload-Content-Type", "video/mp4")
		return req, nil
	}

	resp, err := c.doRetry(reqCtx, build)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", parseAPIError(resp)
	}
	location := resp.Header.Get("Location")
	drainAndClose(resp.Body)
	if strings.TrimSpace(location) == "" {
		return "", fmt.Errorf("youtube: resumable session response has no Location header")
	}
	return location, nil
}

// uploadChunks sends the file from offset and returns the finished video resource
// and the highest confirmed byte offset.
func (c *Client) uploadChunks(ctx context.Context, file *os.File, sessionURI string, offset, total int64, in UploadInput, fileHash string) (videoResource, int64, error) {
	chunk := c.chunkSize
	attempt := 0
	for offset < total {
		if err := ctx.Err(); err != nil {
			return videoResource{}, offset, err
		}
		end := min(offset+chunk, total)
		length := end - offset

		resp, cancel, err := c.putChunkOnce(ctx, sessionURI, file, offset, length, total)
		if err == nil {
			switch resp.StatusCode {
			case http.StatusOK, http.StatusCreated:
				var resource videoResource
				decErr := decodeJSON(resp, &resource)
				cancel()
				if decErr != nil {
					return videoResource{}, offset, decErr
				}
				return resource, total, nil
			// 308 is Google's "Resume Incomplete"; the code coincides with
			// http.StatusPermanentRedirect, which is not what it means here.
			case http.StatusPermanentRedirect:
				rangeHeader := resp.Header.Get("Range")
				retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
				drainAndClose(resp.Body)
				cancel()

				next, ok := parseResumeOffset(rangeHeader)
				if !ok || next < offset || next > end || next > total {
					return videoResource{}, offset, fmt.Errorf("youtube: invalid 308 Range header %q at offset %d", rangeHeader, offset)
				}
				if next > offset {
					offset = next
					attempt = 0
					if err := c.journal.Put(ctx, JournalEntry{
						Key: in.IdempotencyKey, State: UploadStateInProgress, SessionURI: sessionURI,
						TotalBytes: total, SentBytes: offset, FileSHA256: fileHash,
						Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
					}); err != nil {
						return videoResource{}, offset, err
					}
					if retryAfter > 0 {
						if err := c.sleep(ctx, c.backoff(0, retryAfter)); err != nil {
							return videoResource{}, offset, err
						}
					}
					continue
				}
				// Server returned 308 without advancing offset; treat as retryable stall.
				attempt++
				if attempt >= c.maxAttempts {
					return videoResource{}, offset, fmt.Errorf("%w: resumable upload made no progress at offset %d", ErrRetriesExhausted, offset)
				}
				if err := c.sleep(ctx, c.backoff(attempt-1, retryAfter)); err != nil {
					return videoResource{}, offset, err
				}
				continue
			case http.StatusNotFound, http.StatusGone:
				drainAndClose(resp.Body)
				cancel()
				return videoResource{}, offset, fmt.Errorf("%w: status %d", ErrSessionLost, resp.StatusCode)
			default:
				if !retryableStatus(resp.StatusCode) {
					apiErr := parseAPIError(resp)
					cancel()
					return videoResource{}, offset, apiErr
				}
				err = fmt.Errorf("youtube api: status %d", resp.StatusCode)
			}
		}

		// Transport failure or retryable HTTP status: check Retry-After, backoff,
		// and query the session for confirmed progress before retrying.
		var retryAfter time.Duration
		if resp != nil {
			retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
			drainAndClose(resp.Body)
		}
		cancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return videoResource{}, offset, ctxErr
		}
		attempt++
		if attempt >= c.maxAttempts {
			return videoResource{}, offset, fmt.Errorf("%w: %w", ErrRetriesExhausted, err)
		}
		if sleepErr := c.sleep(ctx, c.backoff(attempt-1, retryAfter)); sleepErr != nil {
			return videoResource{}, offset, sleepErr
		}

		outcome, queryErr := c.querySession(ctx, sessionURI, total)
		if queryErr != nil {
			// Status probe failed or session was lost: never re-send bytes
			// from an unconfirmed offset.
			return videoResource{}, offset, queryErr
		}
		if outcome.completed {
			return outcome.resource, total, nil
		}
		if outcome.offset < offset || outcome.offset > total {
			return videoResource{}, offset, fmt.Errorf("youtube: invalid session offset %d for total %d (current %d)", outcome.offset, total, offset)
		}
		if outcome.offset > offset {
			offset = outcome.offset
			attempt = 0
			if putErr := c.journal.Put(ctx, JournalEntry{
				Key: in.IdempotencyKey, State: UploadStateInProgress, SessionURI: sessionURI,
				TotalBytes: total, SentBytes: offset, FileSHA256: fileHash,
				Title: in.Title, Privacy: string(in.Privacy), VideoJobID: in.VideoJobID,
			}); putErr != nil {
				return videoResource{}, offset, putErr
			}
		}
		if outcome.retryAfter > 0 {
			if sleepErr := c.sleep(ctx, c.backoff(0, outcome.retryAfter)); sleepErr != nil {
				return videoResource{}, offset, sleepErr
			}
		}
	}
	return videoResource{}, offset, fmt.Errorf("%w: upload ended without a video id", ErrUploadUnknown)
}

// putChunkOnce sends one byte range with a per-request timeout. The caller must
// invoke the returned CancelFunc after reading and closing the response body.
func (c *Client) putChunkOnce(ctx context.Context, sessionURI string, file *os.File, offset, length, total int64) (*http.Response, context.CancelFunc, error) {
	reqCtx, cancel := c.withTimeout(ctx)

	section := io.NewSectionReader(file, offset, length)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, sessionURI, section)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	req.ContentLength = length
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, total))
	req.Header.Set("Content-Type", "video/mp4")
	resp, err := c.doHTTP(req)
	if err != nil {
		cancel()
		return nil, func() {}, err
	}
	return resp, cancel, nil
}

type sessionOutcome struct {
	completed  bool
	offset     int64
	retryAfter time.Duration
	resource   videoResource
}

// querySession asks the server how many bytes it already holds. It is used both
// to resume and to resolve an ambiguous result without uploading twice.
func (c *Client) querySession(ctx context.Context, sessionURI string, total int64) (sessionOutcome, error) {
	reqCtx, cancel := c.withTimeout(ctx)
	defer cancel()

	build := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPut, sessionURI, nil)
		if err != nil {
			return nil, err
		}
		req.ContentLength = 0
		req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		return req, nil
	}

	resp, err := c.doRetry(reqCtx, build)
	if err != nil {
		return sessionOutcome{}, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var resource videoResource
		if err := decodeJSON(resp, &resource); err != nil {
			return sessionOutcome{}, err
		}
		return sessionOutcome{completed: true, offset: total, resource: resource}, nil
	case http.StatusPermanentRedirect: // 308 Resume Incomplete
		rangeHeader := resp.Header.Get("Range")
		retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
		drainAndClose(resp.Body)
		offset, ok := parseResumeOffset(rangeHeader)
		if !ok {
			return sessionOutcome{}, fmt.Errorf("youtube: invalid 308 Range header %q", rangeHeader)
		}
		return sessionOutcome{offset: offset, retryAfter: retryAfter}, nil
	case http.StatusNotFound, http.StatusGone:
		drainAndClose(resp.Body)
		return sessionOutcome{}, ErrSessionLost
	default:
		return sessionOutcome{}, parseAPIError(resp)
	}
}

// parseResumeOffset reads the last received byte from a 308 Range header.
// "bytes=0-1023" means 1024 bytes are stored, so the next offset is 1024.
// An empty header means 0 bytes have been stored so far.
func parseResumeOffset(header string) (int64, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, true
	}
	switch {
	case strings.HasPrefix(header, "bytes="):
		header = strings.TrimPrefix(header, "bytes=")
	case strings.HasPrefix(header, "bytes "):
		header = strings.TrimPrefix(header, "bytes ")
	default:
		return 0, false
	}
	parts := strings.SplitN(header, "-", 2)
	if len(parts) != 2 {
		return 0, false
	}
	first, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || first != 0 {
		return 0, false
	}
	last, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil || last < 0 {
		return 0, false
	}
	return last + 1, true
}

// WaitProcessing polls videos.list until YouTube reaches a terminal state or
// the timeout expires. Upload completion alone never counts as processing.
func (c *Client) WaitProcessing(ctx context.Context, videoID string, interval time.Duration, timeout time.Duration) (VideoStatus, error) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	deadline := c.now().Add(timeout)
	for {
		status, err := c.GetProcessingStatus(waitCtx, videoID)
		if err != nil {
			return VideoStatus{}, err
		}
		switch status.UploadStatus {
		case "rejected":
			return status, fmt.Errorf("youtube: video %s was rejected: %s", videoID,
				firstNonEmpty(status.RejectionReason, status.FailureReason, "no reason reported"))
		case "failed":
			return status, fmt.Errorf("youtube: upload failed for %s: %s", videoID,
				firstNonEmpty(status.FailureReason, status.RejectionReason, "no reason reported"))
		}
		switch status.ProcessingStatus {
		case "succeeded":
			return status, nil
		case "failed":
			return status, fmt.Errorf("youtube: processing failed for %s: %s", videoID,
				firstNonEmpty(status.FailureReason, status.RejectionReason, "no reason reported"))
		case "terminated":
			return status, fmt.Errorf("youtube: processing terminated for %s: %s", videoID,
				firstNonEmpty(status.FailureReason, status.RejectionReason, "no reason reported"))
		}
		remaining := deadline.Sub(c.now())
		if remaining <= 0 {
			return status, fmt.Errorf("youtube: processing of %s is still %q after %s", videoID, status.ProcessingStatus, timeout)
		}
		sleepFor := interval
		if remaining < sleepFor {
			sleepFor = remaining
		}
		if err := c.sleep(waitCtx, sleepFor); err != nil {
			return status, err
		}
	}
}
