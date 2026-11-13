package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// Additional S3 storage errors.
var (
	ErrObjectNotFound      = errors.New("storage: object not found")
	ErrObjectAlreadyExists = errors.New("storage: object already exists")
	ErrObjectTooLarge      = errors.New("storage: object exceeds max allowed size")
	ErrSizeMismatch        = errors.New("storage: uploaded size does not match declared size")
	ErrInvalidKey          = errors.New("storage: invalid object key")
	ErrInvalidTTL          = errors.New("storage: invalid presign ttl")
)

const maxPresignTTL = 7 * 24 * time.Hour

// Allowed daily key categories.
const (
	CategoryScripts = "scripts"
	CategoryImages  = "images"
	CategoryAudio   = "audio"
	CategoryVideo   = "video"
	CategoryFinal   = "final"
)

// PutResult describes a confirmed object upload.
type PutResult struct {
	Bucket    string
	Key       string
	ETag      string
	SizeBytes int64
	SHA256    string
}

// ObjectStat describes metadata of a stored object.
type ObjectStat struct {
	Bucket       string
	Key          string
	SizeBytes    int64
	ContentType  string
	ETag         string
	LastModified time.Time
}

// S3Store implements S3-compatible object storage (MinIO) using AWS SDK for Go v2
// and satisfies ObjectStorage.
type S3Store struct {
	client         *s3.Client
	presigner      *s3.PresignClient
	defaultBucket  string
	maxObjectBytes int64
	defaultTTL     time.Duration
	requestTimeout time.Duration
}

// NewS3Store builds an S3Store configured for MinIO or any S3-compatible service.
func NewS3Store(ctx context.Context, cfg config.MinIOConfig) (*S3Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("storage: minio endpoint is required")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, ErrMissingBucket
	}

	scheme := "http"
	if cfg.UseSSL {
		scheme = "https"
	}
	endpointURL := cfg.Endpoint
	if !strings.HasPrefix(endpointURL, "http://") && !strings.HasPrefix(endpointURL, "https://") {
		endpointURL = scheme + "://" + endpointURL
	}

	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("storage: load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpointURL)
		o.UsePathStyle = true
	})

	maxBytes := cfg.MaxObjectBytes
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	if cfg.PresignTTL < 0 || cfg.PresignTTL > maxPresignTTL {
		return nil, fmt.Errorf("%w: presign ttl must be between 0 and %s", ErrInvalidTTL, maxPresignTTL)
	}
	ttl := cfg.PresignTTL
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	reqTimeout := cfg.RequestTimeout
	if reqTimeout <= 0 {
		reqTimeout = 30 * time.Second
	}

	return &S3Store{
		client:         client,
		presigner:      s3.NewPresignClient(client),
		defaultBucket:  cfg.Bucket,
		maxObjectBytes: maxBytes,
		defaultTTL:     ttl,
		requestTimeout: reqTimeout,
	}, nil
}

// DefaultBucket returns the configured bucket name.
func (s *S3Store) DefaultBucket() string { return s.defaultBucket }

// EnsureBucket idempotently creates a private bucket if it does not exist.
func (s *S3Store) EnsureBucket(ctx context.Context, bucket string) error {
	if strings.TrimSpace(bucket) == "" {
		bucket = s.defaultBucket
	}
	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	_, err := s.client.HeadBucket(opCtx, &s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	if err == nil {
		return nil
	}
	if !isNotFoundErr(err) {
		return fmt.Errorf("storage: head bucket %q: %w", bucket, err)
	}

	_, err = s.client.CreateBucket(opCtx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
		ACL:    types.BucketCannedACLPrivate,
	})
	if err != nil && !isBucketExistsErr(err) {
		return fmt.Errorf("storage: create bucket %q: %w", bucket, err)
	}
	return nil
}

// Ping verifies connectivity and bucket accessibility with a bounded timeout.
func (s *S3Store) Ping(ctx context.Context) error {
	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	_, err := s.client.HeadBucket(opCtx, &s3.HeadBucketInput{
		Bucket: aws.String(s.defaultBucket),
	})
	if err != nil {
		return fmt.Errorf("storage: ping bucket %q: %w", s.defaultBucket, err)
	}
	return nil
}

// PutObject streams r to S3/MinIO while enforcing maxObjectBytes and computing
// the SHA-256 digest and exact byte size. The caller-supplied reader r must be
// bounded/non-blocking; PutObject checks ctx.Err() before reading, between
// reads, and before network I/O without spawning background reader goroutines.
func (s *S3Store) PutObject(ctx context.Context, obj Object, r io.Reader) (PutResult, error) {
	return s.putObject(ctx, obj, r, false)
}

// PutObjectIfNotExists atomically uploads r only if no object currently exists
// at obj.Bucket/obj.Key (S3 If-None-Match: *). Returns ErrObjectAlreadyExists
// if the key is already present.
func (s *S3Store) PutObjectIfNotExists(ctx context.Context, obj Object, r io.Reader) (PutResult, error) {
	return s.putObject(ctx, obj, r, true)
}

type ctxAwareReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxAwareReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

func (s *S3Store) putObject(ctx context.Context, obj Object, r io.Reader, ifNotExists bool) (PutResult, error) {
	if err := ctx.Err(); err != nil {
		return PutResult{}, fmt.Errorf("storage: put object cancelled: %w", err)
	}
	if obj.Bucket == "" {
		obj.Bucket = s.defaultBucket
	}
	if err := obj.Validate(); err != nil {
		return PutResult{}, err
	}
	if r == nil {
		return PutResult{}, fmt.Errorf("storage: reader is nil")
	}
	if obj.Size > s.maxObjectBytes {
		return PutResult{}, fmt.Errorf("%w: %d > %d", ErrObjectTooLarge, obj.Size, s.maxObjectBytes)
	}

	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	hasher := sha256.New()
	limited := io.LimitReader(&ctxAwareReader{ctx: opCtx, r: r}, s.maxObjectBytes+1)

	var (
		body        io.ReadSeeker
		cleanupFile string
		written     int64
		err         error
	)

	// Buffer small payloads in memory and spill larger streams to a temp file so
	// we can compute SHA-256, enforce maxObjectBytes, and provide a seekable
	// ContentLength body to S3 without unbounded RAM usage.
	const memoryThreshold = 4 << 20 // 4 MiB
	var memBuf bytes.Buffer
	n, err := io.CopyN(&memBuf, io.TeeReader(limited, hasher), memoryThreshold+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return PutResult{}, fmt.Errorf("storage: read object stream: %w", err)
	}
	written = n
	if n <= memoryThreshold {
		body = bytes.NewReader(memBuf.Bytes())
	} else {
		tmp, tmpErr := os.CreateTemp("", "it-news-s3-*")
		if tmpErr != nil {
			return PutResult{}, fmt.Errorf("storage: create spool file: %w", tmpErr)
		}
		cleanupFile = tmp.Name()
		defer func() {
			_ = tmp.Close()
			_ = os.Remove(cleanupFile)
		}()
		if _, wErr := tmp.Write(memBuf.Bytes()); wErr != nil {
			return PutResult{}, fmt.Errorf("storage: write spool prefix: %w", wErr)
		}
		rest, cErr := io.Copy(tmp, io.TeeReader(limited, hasher))
		if cErr != nil {
			return PutResult{}, fmt.Errorf("storage: spool object stream: %w", cErr)
		}
		written += rest
		if _, sErr := tmp.Seek(0, io.SeekStart); sErr != nil {
			return PutResult{}, fmt.Errorf("storage: rewind spool file: %w", sErr)
		}
		body = tmp
	}

	if err := opCtx.Err(); err != nil {
		return PutResult{}, fmt.Errorf("storage: put object cancelled: %w", err)
	}
	if written > s.maxObjectBytes {
		return PutResult{}, fmt.Errorf("%w: %d > %d", ErrObjectTooLarge, written, s.maxObjectBytes)
	}
	if obj.Size > 0 && written != obj.Size {
		return PutResult{}, fmt.Errorf("%w: expected %d, got %d", ErrSizeMismatch, obj.Size, written)
	}

	contentType := strings.TrimSpace(obj.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	input := &s3.PutObjectInput{
		Bucket:        aws.String(obj.Bucket),
		Key:           aws.String(obj.Key),
		Body:          body,
		ContentLength: aws.Int64(written),
		ContentType:   aws.String(contentType),
	}
	if ifNotExists {
		input.IfNoneMatch = aws.String("*")
	}

	out, err := s.client.PutObject(opCtx, input)
	if err != nil {
		if ifNotExists && isPreconditionFailedErr(err) {
			return PutResult{}, fmt.Errorf("%w: %s/%s", ErrObjectAlreadyExists, obj.Bucket, obj.Key)
		}
		return PutResult{}, fmt.Errorf("storage: put object %s/%s: %w", obj.Bucket, obj.Key, err)
	}

	return PutResult{
		Bucket:    obj.Bucket,
		Key:       obj.Key,
		ETag:      strings.Trim(aws.ToString(out.ETag), `"`),
		SizeBytes: written,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err != nil {
		c.cancel()
	}
	return n, err
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// GetObject opens a streaming reader for an object in S3/MinIO, applying
// requestTimeout to both the initial HTTP call and the streamed body lifetime.
func (s *S3Store) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, ObjectStat, error) {
	if bucket == "" {
		bucket = s.defaultBucket
	}
	if strings.TrimSpace(key) == "" {
		return nil, ObjectStat{}, ErrMissingKey
	}

	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	out, err := s.client.GetObject(opCtx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		cancel()
		if isNotFoundErr(err) {
			return nil, ObjectStat{}, fmt.Errorf("%w: %s/%s", ErrObjectNotFound, bucket, key)
		}
		return nil, ObjectStat{}, fmt.Errorf("storage: get object %s/%s: %w", bucket, key, err)
	}

	stat := ObjectStat{
		Bucket:       bucket,
		Key:          key,
		SizeBytes:    aws.ToInt64(out.ContentLength),
		ContentType:  aws.ToString(out.ContentType),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified).UTC(),
	}
	return &cancelReadCloser{ReadCloser: out.Body, cancel: cancel}, stat, nil
}

// StatObject retrieves object metadata without downloading the body.
func (s *S3Store) StatObject(ctx context.Context, bucket, key string) (ObjectStat, error) {
	if bucket == "" {
		bucket = s.defaultBucket
	}
	if strings.TrimSpace(key) == "" {
		return ObjectStat{}, ErrMissingKey
	}

	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	out, err := s.client.HeadObject(opCtx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFoundErr(err) {
			return ObjectStat{}, fmt.Errorf("%w: %s/%s", ErrObjectNotFound, bucket, key)
		}
		return ObjectStat{}, fmt.Errorf("storage: stat object %s/%s: %w", bucket, key, err)
	}
	return ObjectStat{
		Bucket:       bucket,
		Key:          key,
		SizeBytes:    aws.ToInt64(out.ContentLength),
		ContentType:  aws.ToString(out.ContentType),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified).UTC(),
	}, nil
}

// DeleteObject removes an object idempotently (missing objects return nil).
func (s *S3Store) DeleteObject(ctx context.Context, bucket, key string) error {
	if bucket == "" {
		bucket = s.defaultBucket
	}
	if strings.TrimSpace(key) == "" {
		return ErrMissingKey
	}

	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	_, err := s.client.DeleteObject(opCtx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFoundErr(err) {
		return fmt.Errorf("storage: delete object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// ObjectExists reports whether an object exists in S3/MinIO.
func (s *S3Store) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.StatObject(ctx, bucket, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	return false, err
}

// PresignGetObject generates a time-limited presigned GET URL within SigV4 bounds (<= 7 days).
func (s *S3Store) PresignGetObject(ctx context.Context, bucket, key string, ttl time.Duration) (string, error) {
	if bucket == "" {
		bucket = s.defaultBucket
	}
	if strings.TrimSpace(key) == "" {
		return "", ErrMissingKey
	}
	if ttl == 0 {
		ttl = s.defaultTTL
	}
	if ttl <= 0 || ttl > maxPresignTTL {
		return "", fmt.Errorf("%w: %s (must be > 0 and <= %s)", ErrInvalidTTL, ttl, maxPresignTTL)
	}

	opCtx, cancel := context.WithTimeout(ctx, s.requestTimeout)
	defer cancel()

	req, err := s.presigner.PresignGetObject(opCtx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: presign get %s/%s: %w", bucket, key, err)
	}
	return req.URL, nil
}

// Put satisfies ObjectStorage.
func (s *S3Store) Put(ctx context.Context, obj Object, r io.Reader) (ObjectRef, error) {
	res, err := s.PutObject(ctx, obj, r)
	if err != nil {
		return ObjectRef{}, err
	}
	return ObjectRef{Bucket: res.Bucket, Key: res.Key, ETag: res.ETag}, nil
}

// Get satisfies ObjectStorage.
func (s *S3Store) Get(ctx context.Context, ref ObjectRef) (io.ReadCloser, error) {
	body, _, err := s.GetObject(ctx, ref.Bucket, ref.Key)
	return body, err
}

// Delete satisfies ObjectStorage.
func (s *S3Store) Delete(ctx context.Context, ref ObjectRef) error {
	return s.DeleteObject(ctx, ref.Bucket, ref.Key)
}

// PresignGet satisfies ObjectStorage.
func (s *S3Store) PresignGet(ctx context.Context, ref ObjectRef, ttl time.Duration) (string, error) {
	return s.PresignGetObject(ctx, ref.Bucket, ref.Key, ttl)
}

// BuildDailyObjectKey builds a normalized, traversal-safe, collision-free key under
// daily/YYYY-MM-DD/{video_job_id}/{category}/{sanitized_filename}.
func BuildDailyObjectKey(date time.Time, jobID domain.VideoJobID, category, filename string) (string, error) {
	if date.IsZero() {
		return "", fmt.Errorf("%w: date is required", ErrInvalidKey)
	}
	safeJobID, err := validateJobIDSegment(jobID.String())
	if err != nil {
		return "", err
	}
	switch category {
	case CategoryScripts, CategoryImages, CategoryAudio, CategoryVideo, CategoryFinal:
	default:
		return "", fmt.Errorf("%w: unsupported category %q", ErrInvalidKey, category)
	}

	safeName, err := sanitizeFilename(filename)
	if err != nil {
		return "", err
	}
	key := path.Join("daily", date.UTC().Format("2006-01-02"), safeJobID, category, safeName)
	if !strings.HasPrefix(key, "daily/"+date.UTC().Format("2006-01-02")+"/") {
		return "", fmt.Errorf("%w: key escaped daily namespace", ErrInvalidKey)
	}
	return key, nil
}

func validateJobIDSegment(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed != raw {
		return "", fmt.Errorf("%w: video_job_id is invalid", ErrInvalidKey)
	}
	if trimmed == "." || trimmed == ".." || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, "/\\") {
		return "", fmt.Errorf("%w: video_job_id contains forbidden path sequence", ErrInvalidKey)
	}
	for _, r := range trimmed {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return "", fmt.Errorf("%w: video_job_id contains unsupported character %q", ErrInvalidKey, r)
		}
	}
	return trimmed, nil
}

func sanitizeFilename(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "." || trimmed == ".." || strings.Contains(trimmed, "..") || strings.ContainsAny(trimmed, "/\\\x00") {
		return "", fmt.Errorf("%w: invalid filename", ErrInvalidKey)
	}
	var b strings.Builder
	b.Grow(len(trimmed))
	changed := trimmed != raw
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
			changed = true
		}
	}
	cleaned := strings.Trim(b.String(), ".")
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "", fmt.Errorf("%w: invalid filename", ErrInvalidKey)
	}
	if cleaned != trimmed {
		changed = true
	}
	if !changed {
		return cleaned, nil
	}
	// Append a deterministic hash suffix whenever normalization modified the raw
	// filename so distinct inputs cannot collide lossily.
	sum := sha256.Sum256([]byte(raw))
	ext := path.Ext(cleaned)
	stem := strings.TrimSuffix(cleaned, ext)
	if stem == "" {
		stem = "file"
	}
	return fmt.Sprintf("%s-%s%s", stem, hex.EncodeToString(sum[:4]), ext), nil
}

// ReconcileOrphans finds objects under prefix that are older than olderThan
// and not known/protected by isProtected (which must check both committed
// metadata and any active/in-flight workflow runs). When dryRun is false, it
// re-verifies object ETag/LastModified and isProtected immediately before
// deleting so concurrent/in-flight uploads are never removed blindly.
func (s *S3Store) ReconcileOrphans(
	ctx context.Context,
	bucket, prefix string,
	olderThan time.Duration,
	dryRun bool,
	isProtected func(ctx context.Context, bucket, key string) (bool, error),
) ([]string, error) {
	if bucket == "" {
		bucket = s.defaultBucket
	}
	if isProtected == nil {
		return nil, fmt.Errorf("storage: isProtected predicate is required")
	}
	if olderThan <= 0 {
		return nil, fmt.Errorf("storage: olderThan must be positive for safe orphan reconciliation")
	}

	cutoff := time.Now().UTC().Add(-olderThan)
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	var orphans []string
	for paginator.HasMorePages() {
		if err := ctx.Err(); err != nil {
			return orphans, err
		}
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("storage: list objects for reconciliation: %w", err)
		}
		for _, item := range page.Contents {
			if err := ctx.Err(); err != nil {
				return orphans, err
			}
			key := aws.ToString(item.Key)
			if aws.ToTime(item.LastModified).UTC().After(cutoff) {
				continue
			}
			protected, err := isProtected(ctx, bucket, key)
			if err != nil {
				return nil, fmt.Errorf("storage: check object %q in database: %w", key, err)
			}
			if protected {
				continue
			}
			if !dryRun {
				// Re-stat and re-verify protection immediately before delete to avoid
				// racing with an in-flight upload or concurrent metadata commit.
				stat, statErr := s.StatObject(ctx, bucket, key)
				if errors.Is(statErr, ErrObjectNotFound) {
					continue
				}
				if statErr != nil {
					return orphans, statErr
				}
				if stat.LastModified.After(cutoff) || stat.ETag != strings.Trim(aws.ToString(item.ETag), `"`) {
					continue
				}
				protectedAgain, err := isProtected(ctx, bucket, key)
				if err != nil {
					return orphans, fmt.Errorf("storage: re-check object %q in database: %w", key, err)
				}
				if protectedAgain {
					continue
				}
				if err := s.DeleteObject(ctx, bucket, key); err != nil {
					return orphans, err
				}
			}
			orphans = append(orphans, key)
		}
	}
	return orphans, nil
}

func isNotFoundErr(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "NotFound" || code == "NoSuchKey" || code == "NoSuchBucket" {
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}

func isBucketExistsErr(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		return code == "BucketAlreadyOwnedByYou" || code == "BucketAlreadyExists"
	}
	return false
}

func isPreconditionFailedErr(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "PreconditionFailed" || code == "ConditionalRequestConflict" {
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusPreconditionFailed {
		return true
	}
	return false
}
