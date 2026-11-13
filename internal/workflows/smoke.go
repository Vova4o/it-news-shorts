package workflows

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// SmokeWorkflowName is the registered Temporal workflow name.
const SmokeWorkflowName = "InfrastructureSmokeWorkflow"

// InfrastructureSmokeInput is the deterministic input for
// InfrastructureSmokeWorkflow. RunID must be generated outside the workflow.
type InfrastructureSmokeInput struct {
	RunID           string        `json:"run_id"`
	TestName        string        `json:"test_name"`
	ArtifactContent string        `json:"artifact_content"`
	Delay           time.Duration `json:"delay,omitempty"`
	ActivityTimeout time.Duration `json:"activity_timeout,omitempty"`
	MaxAttempts     int32         `json:"max_attempts,omitempty"`
}

// Validate checks deterministic input invariants.
func (in InfrastructureSmokeInput) Validate() error {
	if strings.TrimSpace(in.RunID) == "" {
		return fmt.Errorf("workflows: run_id is required")
	}
	if strings.TrimSpace(in.TestName) == "" {
		return fmt.Errorf("workflows: test_name is required")
	}
	if in.ArtifactContent == "" {
		return fmt.Errorf("workflows: artifact_content is required")
	}
	return nil
}

// InfrastructureSmokeResult is returned by InfrastructureSmokeWorkflow.
type InfrastructureSmokeResult struct {
	Status            string `json:"status"`
	RunID             string `json:"run_id"`
	ArtifactKey       string `json:"artifact_key"`
	ChecksumVerified  bool   `json:"checksum_verified"`
	DatabasePersisted bool   `json:"database_persisted"`
}

// CreateSmokeRunInput is the activity input for CreateSmokeRun.
type CreateSmokeRunInput struct {
	RunID         string    `json:"run_id"`
	TestName      string    `json:"test_name"`
	PayloadSHA256 string    `json:"payload_sha256"`
	WorkflowID    string    `json:"workflow_id"`
	Now           time.Time `json:"now"`
}

// CreateSmokeRunOutput is returned by CreateSmokeRun.
type CreateSmokeRunOutput struct {
	DailyRunID       string `json:"daily_run_id"`
	StoryID          string `json:"story_id"`
	VideoJobID       string `json:"video_job_id"`
	AlreadyCompleted bool   `json:"already_completed,omitempty"`
}

// UploadArtifactInput is the activity input for UploadAndVerifyArtifact.
type UploadArtifactInput struct {
	RunID      string        `json:"run_id"`
	VideoJobID string        `json:"video_job_id"`
	WorkflowID string        `json:"workflow_id"`
	Content    string        `json:"content"`
	Delay      time.Duration `json:"delay,omitempty"`
}

// UploadArtifactOutput is returned by UploadAndVerifyArtifact.
type UploadArtifactOutput struct {
	Bucket           string `json:"bucket"`
	Key              string `json:"key"`
	SizeBytes        int64  `json:"size_bytes"`
	Checksum         string `json:"checksum"`
	ChecksumVerified bool   `json:"checksum_verified"`
}

// PersistMediaAssetInput is the activity input for PersistMediaAsset.
type PersistMediaAssetInput struct {
	RunID      string    `json:"run_id"`
	VideoJobID string    `json:"video_job_id"`
	WorkflowID string    `json:"workflow_id"`
	Bucket     string    `json:"bucket"`
	Key        string    `json:"key"`
	SizeBytes  int64     `json:"size_bytes"`
	Checksum   string    `json:"checksum"`
	Now        time.Time `json:"now"`
}

// CompleteSmokeRunInput is the activity input for CompleteSmokeRun.
type CompleteSmokeRunInput struct {
	RunID      string    `json:"run_id"`
	WorkflowID string    `json:"workflow_id"`
	Now        time.Time `json:"now"`
}

// FailSmokeRunInput is the activity input for FailSmokeRun.
type FailSmokeRunInput struct {
	RunID        string `json:"run_id"`
	WorkflowID   string `json:"workflow_id"`
	ErrorMessage string `json:"error_message"`
}

// InfrastructureSmokeWorkflow verifies end-to-end PostgreSQL and MinIO
// persistence through deterministic Temporal activities.
func InfrastructureSmokeWorkflow(ctx workflow.Context, in InfrastructureSmokeInput) (InfrastructureSmokeResult, error) {
	if err := in.Validate(); err != nil {
		return InfrastructureSmokeResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "PermanentError", err)
	}

	info := workflow.GetInfo(ctx)
	wfNow := workflow.Now(ctx).UTC()
	logger := workflow.GetLogger(ctx)
	logger.Info("starting InfrastructureSmokeWorkflow",
		"workflow_id", info.WorkflowExecution.ID,
		"run_id", in.RunID,
		"test_name", in.TestName,
	)

	actTimeout := in.ActivityTimeout
	if actTimeout <= 0 {
		actTimeout = 30 * time.Second
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 5
	}

	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: actTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    10 * time.Second,
			MaximumAttempts:    maxAttempts,
			NonRetryableErrorTypes: []string{
				"PermanentError",
			},
		},
	})

	sum := sha256.Sum256([]byte(in.ArtifactContent))
	payloadSHA := hex.EncodeToString(sum[:])

	var runOut CreateSmokeRunOutput
	if err := workflow.ExecuteActivity(actCtx, "CreateSmokeRun", CreateSmokeRunInput{
		RunID:         in.RunID,
		TestName:      in.TestName,
		PayloadSHA256: payloadSHA,
		WorkflowID:    info.WorkflowExecution.ID,
		Now:           wfNow,
	}).Get(ctx, &runOut); err != nil {
		return InfrastructureSmokeResult{}, err
	}

	markFailed := func(cause error) {
		disconnCtx, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		failCtx := workflow.WithActivityOptions(disconnCtx, workflow.ActivityOptions{
			StartToCloseTimeout: 5 * time.Second,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 2,
			},
		})
		if failErr := workflow.ExecuteActivity(failCtx, "FailSmokeRun", FailSmokeRunInput{
			RunID:        in.RunID,
			WorkflowID:   info.WorkflowExecution.ID,
			ErrorMessage: cause.Error(),
		}).Get(failCtx, nil); failErr != nil {
			logger.Error("failed to persist smoke run failure state",
				"run_id", in.RunID,
				"workflow_id", info.WorkflowExecution.ID,
				"original_error", cause.Error(),
				"cleanup_error", failErr.Error(),
			)
		}
	}

	var uploadOut UploadArtifactOutput
	if err := workflow.ExecuteActivity(actCtx, "UploadAndVerifyArtifact", UploadArtifactInput{
		RunID:      in.RunID,
		VideoJobID: runOut.VideoJobID,
		WorkflowID: info.WorkflowExecution.ID,
		Content:    in.ArtifactContent,
		Delay:      in.Delay,
	}).Get(ctx, &uploadOut); err != nil {
		markFailed(err)
		return InfrastructureSmokeResult{}, err
	}

	if err := workflow.ExecuteActivity(actCtx, "PersistMediaAsset", PersistMediaAssetInput{
		RunID:      in.RunID,
		VideoJobID: runOut.VideoJobID,
		WorkflowID: info.WorkflowExecution.ID,
		Bucket:     uploadOut.Bucket,
		Key:        uploadOut.Key,
		SizeBytes:  uploadOut.SizeBytes,
		Checksum:   uploadOut.Checksum,
		Now:        wfNow,
	}).Get(ctx, nil); err != nil {
		markFailed(err)
		return InfrastructureSmokeResult{}, err
	}

	if !runOut.AlreadyCompleted {
		if err := workflow.ExecuteActivity(actCtx, "CompleteSmokeRun", CompleteSmokeRunInput{
			RunID:      in.RunID,
			WorkflowID: info.WorkflowExecution.ID,
			Now:        workflow.Now(ctx).UTC(),
		}).Get(ctx, nil); err != nil {
			markFailed(err)
			return InfrastructureSmokeResult{}, err
		}
	}

	return InfrastructureSmokeResult{
		Status:            "COMPLETED",
		RunID:             in.RunID,
		ArtifactKey:       uploadOut.Key,
		ChecksumVerified:  uploadOut.ChecksumVerified,
		DatabasePersisted: true,
	}, nil
}
