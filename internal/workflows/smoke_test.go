package workflows_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func registerMockSmokeActivities(
	env *testsuite.TestWorkflowEnvironment,
	createFn func(context.Context, workflows.CreateSmokeRunInput) (workflows.CreateSmokeRunOutput, error),
	uploadFn func(context.Context, workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error),
	persistFn func(context.Context, workflows.PersistMediaAssetInput) error,
	completeFn func(context.Context, workflows.CompleteSmokeRunInput) error,
) {
	if createFn == nil {
		createFn = func(_ context.Context, in workflows.CreateSmokeRunInput) (workflows.CreateSmokeRunOutput, error) {
			return workflows.CreateSmokeRunOutput{
				DailyRunID: in.RunID,
				StoryID:    "smoke-story-" + in.RunID,
				VideoJobID: "smoke-job-" + in.RunID,
			}, nil
		}
	}
	if uploadFn == nil {
		uploadFn = func(_ context.Context, in workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error) {
			return workflows.UploadArtifactOutput{
				Bucket:           "it-news-media",
				Key:              "smoke-tests/" + in.RunID + "/result.txt",
				SizeBytes:        int64(len(in.Content)),
				Checksum:         "deadbeef",
				ChecksumVerified: true,
			}, nil
		}
	}
	if persistFn == nil {
		persistFn = func(context.Context, workflows.PersistMediaAssetInput) error { return nil }
	}
	if completeFn == nil {
		completeFn = func(context.Context, workflows.CompleteSmokeRunInput) error { return nil }
	}

	env.RegisterActivityWithOptions(createFn, activity.RegisterOptions{Name: "CreateSmokeRun"})
	env.RegisterActivityWithOptions(uploadFn, activity.RegisterOptions{Name: "UploadAndVerifyArtifact"})
	env.RegisterActivityWithOptions(persistFn, activity.RegisterOptions{Name: "PersistMediaAsset"})
	env.RegisterActivityWithOptions(completeFn, activity.RegisterOptions{Name: "CompleteSmokeRun"})
	env.RegisterActivityWithOptions(func(context.Context, workflows.FailSmokeRunInput) error { return nil }, activity.RegisterOptions{Name: "FailSmokeRun"})
}

func TestInfrastructureSmokeWorkflowSuccess(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	registerMockSmokeActivities(env, nil, nil, nil, nil)

	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-001",
		TestName:        "unit-success",
		ArtifactContent: "hello smoke",
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	var res workflows.InfrastructureSmokeResult
	if err := env.GetWorkflowResult(&res); err != nil {
		t.Fatalf("GetWorkflowResult() error = %v", err)
	}
	if res.Status != "COMPLETED" || !res.ChecksumVerified || !res.DatabasePersisted {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.ArtifactKey != "smoke-tests/run-001/result.txt" {
		t.Fatalf("ArtifactKey = %q, want smoke-tests/run-001/result.txt", res.ArtifactKey)
	}
}

func TestInfrastructureSmokeWorkflowRetriesTransientMinIOError(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var attempts atomic.Int32
	uploadFn := func(_ context.Context, in workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error) {
		n := attempts.Add(1)
		if n < 3 {
			return workflows.UploadArtifactOutput{}, errors.New("temporary minio connection reset")
		}
		return workflows.UploadArtifactOutput{
			Bucket:           "it-news-media",
			Key:              "smoke-tests/" + in.RunID + "/result.txt",
			SizeBytes:        int64(len(in.Content)),
			Checksum:         "abc123",
			ChecksumVerified: true,
		}, nil
	}

	registerMockSmokeActivities(env, nil, uploadFn, nil, nil)
	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-retry",
		TestName:        "retry-minio",
		ArtifactContent: "retry payload",
	})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error = %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestInfrastructureSmokeWorkflowDoesNotRetryPermanentError(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var attempts atomic.Int32
	createFn := func(context.Context, workflows.CreateSmokeRunInput) (workflows.CreateSmokeRunOutput, error) {
		attempts.Add(1)
		return workflows.CreateSmokeRunOutput{}, temporal.NewNonRetryableApplicationError("invalid run", "PermanentError", errors.New("validation failed"))
	}

	registerMockSmokeActivities(env, createFn, nil, nil, nil)
	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-perm",
		TestName:        "perm-err",
		ArtifactContent: "payload",
	})

	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("expected workflow error, got nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry for PermanentError)", got)
	}
}

func TestInfrastructureSmokeWorkflowTimeout(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	uploadFn := func(ctx context.Context, in workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error) {
		return workflows.UploadArtifactOutput{}, temporal.NewTimeoutError(enums.TIMEOUT_TYPE_START_TO_CLOSE, context.DeadlineExceeded)
	}

	registerMockSmokeActivities(env, nil, uploadFn, nil, nil)
	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-timeout",
		TestName:        "timeout",
		ArtifactContent: "payload",
	})

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !temporal.IsTimeoutError(err) {
		t.Fatalf("expected TimeoutError, got %v", err)
	}
}

func TestInfrastructureSmokeWorkflowCancellation(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	uploadFn := func(ctx context.Context, in workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error) {
		<-ctx.Done()
		return workflows.UploadArtifactOutput{}, ctx.Err()
	}

	registerMockSmokeActivities(env, nil, uploadFn, nil, nil)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, 200*time.Millisecond)

	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-cancel",
		TestName:        "cancel",
		ArtifactContent: "payload",
	})

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	if !temporal.IsCanceledError(err) {
		t.Fatalf("expected canceled error, got %v", err)
	}
}

func TestInfrastructureSmokeWorkflowCleanupFailurePreservesOriginalError(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var failCalled atomic.Int32
	uploadFn := func(context.Context, workflows.UploadArtifactInput) (workflows.UploadArtifactOutput, error) {
		return workflows.UploadArtifactOutput{}, temporal.NewNonRetryableApplicationError("upload permanent failure", "PermanentError", nil)
	}
	failFn := func(context.Context, workflows.FailSmokeRunInput) error {
		failCalled.Add(1)
		return errors.New("postgres unavailable during cleanup")
	}

	env.RegisterActivityWithOptions(func(_ context.Context, in workflows.CreateSmokeRunInput) (workflows.CreateSmokeRunOutput, error) {
		return workflows.CreateSmokeRunOutput{
			DailyRunID: in.RunID,
			StoryID:    "smoke-story-" + in.RunID,
			VideoJobID: "smoke-job-" + in.RunID,
		}, nil
	}, activity.RegisterOptions{Name: "CreateSmokeRun"})
	env.RegisterActivityWithOptions(uploadFn, activity.RegisterOptions{Name: "UploadAndVerifyArtifact"})
	env.RegisterActivityWithOptions(func(context.Context, workflows.PersistMediaAssetInput) error { return nil }, activity.RegisterOptions{Name: "PersistMediaAsset"})
	env.RegisterActivityWithOptions(func(context.Context, workflows.CompleteSmokeRunInput) error { return nil }, activity.RegisterOptions{Name: "CompleteSmokeRun"})
	env.RegisterActivityWithOptions(failFn, activity.RegisterOptions{Name: "FailSmokeRun"})

	env.ExecuteWorkflow(workflows.InfrastructureSmokeWorkflow, workflows.InfrastructureSmokeInput{
		RunID:           "run-cleanup-fail",
		TestName:        "cleanup-fail",
		ArtifactContent: "payload",
	})

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("expected workflow error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != "PermanentError" {
		t.Fatalf("expected original PermanentError to be preserved, got %v", err)
	}
	if failCalled.Load() != 2 {
		t.Fatalf("FailSmokeRun attempts = %d, want 2", failCalled.Load())
	}
}
