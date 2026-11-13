package workflows

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
)

// SmokeActivities executes PostgreSQL and MinIO operations for
// InfrastructureSmokeWorkflow.
type SmokeActivities struct {
	DailyRuns   *postgres.DailyRunRepository
	Stories     *postgres.StoryRepository
	VideoJobs   *postgres.VideoJobRepository
	MediaAssets *postgres.MediaAssetRepository
	Storage     *storage.S3Store
	Logger      *slog.Logger
	Metrics     *observability.Metrics
}

// CreateSmokeRun idempotently creates DailyRun, Story, and VideoJob rows in
// PostgreSQL so all foreign-key constraints are satisfied.
func (a *SmokeActivities) CreateSmokeRun(ctx context.Context, in CreateSmokeRunInput) (CreateSmokeRunOutput, error) {
	const name = "CreateSmokeRun"
	start := time.Now()
	info := activity.GetInfo(ctx)
	a.recordRetry(name, info.Attempt)

	var err error
	defer func() { a.finishActivity(name, info.WorkflowExecution.ID, in.RunID, "", start, err) }()

	if strings.TrimSpace(in.RunID) == "" {
		err = permanentError(domain.MissingField("run_id"))
		return CreateSmokeRunOutput{}, err
	}
	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	runID, err := domain.NewDailyRunID(in.RunID)
	if err != nil {
		err = permanentError(err)
		return CreateSmokeRunOutput{}, err
	}
	storyID, err := domain.NewStoryID("smoke-story-" + in.RunID)
	if err != nil {
		err = permanentError(err)
		return CreateSmokeRunOutput{}, err
	}
	jobID, err := domain.NewVideoJobID("smoke-job-" + in.RunID)
	if err != nil {
		err = permanentError(err)
		return CreateSmokeRunOutput{}, err
	}
	idemKey, err := domain.NewIdempotencyKey("smoke/" + in.RunID + "/v1")
	if err != nil {
		err = permanentError(err)
		return CreateSmokeRunOutput{}, err
	}

	runDate := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.UTC)
	run := domain.DailyRun{
		ID:      runID,
		RunDate: runDate,
		Window: domain.TimeWindow{
			Start: runDate.Add(-domain.NewsWindow),
			End:   runDate,
		},
		State:      domain.DailyRunRunning,
		MaxStories: 1,
		CreatedAt:  now,
	}

	depStart := time.Now()
	fpSum := sha256.Sum256([]byte(in.TestName + "\n" + in.PayloadSHA256))
	fingerprint := hex.EncodeToString(fpSum[:])

	story := domain.Story{
		ID:         storyID,
		DailyRunID: runID,
		Title:      "Smoke Test: " + in.TestName,
		Summary:    "Infrastructure smoke verification story",
		Score:      1.0,
		Position:   1,
		Evidence: []domain.StoryEvidence{
			{
				ArticleID:   domain.ArticleID("smoke-article-" + in.RunID),
				Source:      "smoke-test",
				URL:         "https://example.invalid/smoke/" + in.RunID,
				Quote:       "Deterministic infrastructure smoke verification evidence.",
				PublishedAt: now,
			},
		},
		CreatedAt: now,
	}

	job, err := domain.NewVideoJob(domain.NewVideoJobParams{
		ID:             jobID,
		DailyRunID:     runID,
		StoryID:        storyID,
		IdempotencyKey: idemKey,
		MaxAttempts:    domain.DefaultMaxAttempts,
		CreatedAt:      now,
	})
	if err != nil {
		err = permanentError(err)
		return CreateSmokeRunOutput{}, err
	}

	rec, created, err := a.DailyRuns.CreateSmokeGraphIdempotent(ctx, run, story, job.Snapshot(), in.WorkflowID, fingerprint)
	a.Metrics.ObserveDependency("postgres", "create_smoke_graph", time.Since(depStart), err)
	if err != nil {
		classified := classifyPGError(err)
		if a.Metrics != nil && temporal.IsApplicationError(classified) {
			a.Metrics.WorkflowTotal.WithLabelValues(SmokeWorkflowName, "failed").Inc()
		}
		return CreateSmokeRunOutput{}, classified
	}

	return CreateSmokeRunOutput{
		DailyRunID:       runID.String(),
		StoryID:          storyID.String(),
		VideoJobID:       jobID.String(),
		AlreadyCompleted: !created && rec.Run.State == domain.DailyRunCompleted,
	}, nil
}

// UploadAndVerifyArtifact writes the smoke artifact to MinIO, downloads it
// back, and verifies byte size and SHA-256 integrity.
func (a *SmokeActivities) UploadAndVerifyArtifact(ctx context.Context, in UploadArtifactInput) (UploadArtifactOutput, error) {
	const name = "UploadAndVerifyArtifact"
	start := time.Now()
	info := activity.GetInfo(ctx)
	a.recordRetry(name, info.Attempt)

	var err error
	defer func() { a.finishActivity(name, info.WorkflowExecution.ID, in.RunID, in.VideoJobID, start, err) }()

	if strings.TrimSpace(in.RunID) == "" || in.Content == "" {
		err = permanentError(fmt.Errorf("workflows: run_id and content are required"))
		return UploadArtifactOutput{}, err
	}

	if in.Delay > 0 {
		timer := time.NewTimer(in.Delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			err = ctx.Err()
			return UploadArtifactOutput{}, err
		case <-timer.C:
		}
	}

	key := fmt.Sprintf("smoke-tests/%s/result.txt", in.RunID)
	bucket := a.Storage.DefaultBucket()
	payload := []byte(in.Content)
	expectedSum := sha256.Sum256(payload)
	expectedSHA := hex.EncodeToString(expectedSum[:])
	expectedSize := int64(len(payload))

	depStart := time.Now()
	putRes, err := a.Storage.PutObjectIfNotExists(ctx, storage.Object{
		Bucket:      bucket,
		Key:         key,
		ContentType: "text/plain; charset=utf-8",
		Size:        expectedSize,
	}, bytes.NewReader(payload))
	if errors.Is(err, storage.ErrObjectAlreadyExists) {
		a.Metrics.ObserveDependency("minio", "put_object", time.Since(depStart), nil)
		putRes = storage.PutResult{
			Bucket:    bucket,
			Key:       key,
			SizeBytes: expectedSize,
			SHA256:    expectedSHA,
		}
		err = nil
	} else {
		a.Metrics.ObserveDependency("minio", "put_object", time.Since(depStart), err)
	}
	if err != nil {
		return UploadArtifactOutput{}, classifyStorageError(err)
	}

	depStart = time.Now()
	body, stat, err := a.Storage.GetObject(ctx, bucket, key)
	if err != nil {
		a.Metrics.ObserveDependency("minio", "get_object", time.Since(depStart), err)
		return UploadArtifactOutput{}, classifyStorageError(err)
	}
	hasher := sha256.New()
	readBytes, readErr := io.Copy(hasher, body)
	closeErr := body.Close()
	if readErr != nil {
		a.Metrics.ObserveDependency("minio", "get_object", time.Since(depStart), readErr)
		err = readErr
		return UploadArtifactOutput{}, err
	}
	if closeErr != nil {
		a.Metrics.ObserveDependency("minio", "get_object", time.Since(depStart), closeErr)
		err = closeErr
		return UploadArtifactOutput{}, err
	}
	a.Metrics.ObserveDependency("minio", "get_object", time.Since(depStart), nil)

	actualSHA := hex.EncodeToString(hasher.Sum(nil))
	if actualSHA != putRes.SHA256 || readBytes != putRes.SizeBytes || stat.SizeBytes != putRes.SizeBytes {
		err = permanentError(fmt.Errorf("workflows: artifact %s/%s conflict (stored sha=%s size=%d, expected sha=%s size=%d)",
			bucket, key, actualSHA, readBytes, expectedSHA, expectedSize))
		return UploadArtifactOutput{}, err
	}

	return UploadArtifactOutput{
		Bucket:           bucket,
		Key:              key,
		SizeBytes:        putRes.SizeBytes,
		Checksum:         putRes.SHA256,
		ChecksumVerified: true,
	}, nil
}

// PersistMediaAsset records MediaAsset metadata in PostgreSQL after MinIO
// upload and read-back verification succeeded.
func (a *SmokeActivities) PersistMediaAsset(ctx context.Context, in PersistMediaAssetInput) error {
	const name = "PersistMediaAsset"
	start := time.Now()
	info := activity.GetInfo(ctx)
	a.recordRetry(name, info.Attempt)

	var err error
	defer func() { a.finishActivity(name, info.WorkflowExecution.ID, in.RunID, in.VideoJobID, start, err) }()

	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	assetID, err := domain.NewMediaAssetID("smoke-asset-" + in.RunID)
	if err != nil {
		return permanentError(err)
	}
	jobID, err := domain.NewVideoJobID(in.VideoJobID)
	if err != nil {
		return permanentError(err)
	}

	asset := domain.MediaAsset{
		ID:         assetID,
		VideoJobID: jobID,
		Kind:       domain.MediaSmokeArtifact,
		URI:        fmt.Sprintf("s3://%s/%s", in.Bucket, in.Key),
		MIMEType:   "text/plain; charset=utf-8",
		SizeBytes:  in.SizeBytes,
		Checksum:   in.Checksum,
		Provider:   "minio-smoke",
		CreatedAt:  now,
	}

	depStart := time.Now()
	_, _, err = a.MediaAssets.CreateIdempotent(ctx, asset, in.Bucket, in.Key)
	a.Metrics.ObserveDependency("postgres", "create_media_asset", time.Since(depStart), err)
	if err != nil {
		return classifyPGError(err)
	}
	return nil
}

// CompleteSmokeRun marks the DailyRun as COMPLETED in PostgreSQL.
func (a *SmokeActivities) CompleteSmokeRun(ctx context.Context, in CompleteSmokeRunInput) error {
	const name = "CompleteSmokeRun"
	start := time.Now()
	info := activity.GetInfo(ctx)
	a.recordRetry(name, info.Attempt)

	var err error
	defer func() { a.finishActivity(name, info.WorkflowExecution.ID, in.RunID, "", start, err) }()

	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	runID, err := domain.NewDailyRunID(in.RunID)
	if err != nil {
		return permanentError(err)
	}

	depStart := time.Now()
	_, err = a.DailyRuns.UpdateStatus(ctx, runID, domain.DailyRunCompleted, in.WorkflowID, "", now)
	a.Metrics.ObserveDependency("postgres", "complete_daily_run", time.Since(depStart), err)
	if err != nil {
		return classifyPGError(err)
	}
	if a.Metrics != nil {
		a.Metrics.WorkflowTotal.WithLabelValues(SmokeWorkflowName, "completed").Inc()
	}
	return nil
}

// FailSmokeRun marks an owned RUNNING DailyRun as FAILED in PostgreSQL.
func (a *SmokeActivities) FailSmokeRun(ctx context.Context, in FailSmokeRunInput) error {
	const name = "FailSmokeRun"
	start := time.Now()
	info := activity.GetInfo(ctx)
	a.recordRetry(name, info.Attempt)

	var err error
	defer func() { a.finishActivity(name, info.WorkflowExecution.ID, in.RunID, "", start, err) }()

	runID, err := domain.NewDailyRunID(in.RunID)
	if err != nil {
		return permanentError(err)
	}
	depStart := time.Now()
	err = a.DailyRuns.MarkFailedIfOwned(ctx, runID, in.WorkflowID, in.ErrorMessage)
	a.Metrics.ObserveDependency("postgres", "fail_daily_run", time.Since(depStart), err)
	if err != nil {
		return classifyPGError(err)
	}
	if a.Metrics != nil {
		a.Metrics.WorkflowTotal.WithLabelValues(SmokeWorkflowName, "failed").Inc()
	}
	return nil
}

func (a *SmokeActivities) recordRetry(activityName string, attempt int32) {
	if a.Metrics != nil && attempt > 1 {
		a.Metrics.ActivityRetryTotal.WithLabelValues(activityName).Inc()
	}
}

func (a *SmokeActivities) finishActivity(activityName, workflowID, runID, videoJobID string, start time.Time, err error) {
	elapsed := time.Since(start)
	status := "ok"
	if err != nil {
		status = "error"
	}
	if a.Metrics != nil {
		a.Metrics.ActivityDuration.WithLabelValues(activityName, status).Observe(elapsed.Seconds())
	}
	if a.Logger != nil {
		attrs := []any{
			"workflow_id", workflowID,
			"run_id", runID,
			"video_job_id", videoJobID,
			"activity", activityName,
			"duration_ms", elapsed.Milliseconds(),
		}
		if err != nil {
			attrs = append(attrs, "error", err.Error())
			a.Logger.Error("activity failed", attrs...)
			return
		}
		a.Logger.Info("activity completed", attrs...)
	}
}

func permanentError(err error) error {
	return temporal.NewNonRetryableApplicationError(err.Error(), "PermanentError", err)
}

func classifyPGError(err error) error {
	if errors.Is(err, domain.ErrMissingField) ||
		errors.Is(err, domain.ErrInvalidValue) ||
		errors.Is(err, domain.ErrInvalidTransition) ||
		errors.Is(err, postgres.ErrForeignKey) ||
		errors.Is(err, postgres.ErrIdempotencyConflict) {
		return permanentError(err)
	}
	return err
}

func classifyStorageError(err error) error {
	if errors.Is(err, storage.ErrMissingBucket) ||
		errors.Is(err, storage.ErrMissingKey) ||
		errors.Is(err, storage.ErrNegativeSize) ||
		errors.Is(err, storage.ErrObjectTooLarge) ||
		errors.Is(err, storage.ErrSizeMismatch) ||
		errors.Is(err, storage.ErrInvalidKey) {
		return permanentError(err)
	}
	return err
}
