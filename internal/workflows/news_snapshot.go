package workflows

import (
	"fmt"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// DailyNewsSnapshotWorkflowName is the registered Temporal workflow name.
const DailyNewsSnapshotWorkflowName = "DailyNewsSnapshotWorkflow"

// DailyNewsSnapshotInput configures a 24h daily snapshot in Europe/Moscow.
type DailyNewsSnapshotInput struct {
	SnapshotDate             string `json:"snapshot_date,omitempty"` // YYYY-MM-DD in Europe/Moscow
	IncludeSubmittedFallback bool   `json:"include_submitted_fallback,omitempty"`
}

// ComputeMoscowSnapshotWindow calculates the [Start, End) 24h window ending at
// 09:00 Europe/Moscow on the target date.
func ComputeMoscowSnapshotWindow(dateStr string, now time.Time) (time.Time, domain.TimeWindow, error) {
	msk := domain.Moscow()
	var target time.Time
	if strings.TrimSpace(dateStr) != "" {
		parsed, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(dateStr), msk)
		if err != nil {
			return time.Time{}, domain.TimeWindow{}, fmt.Errorf("invalid snapshot_date %q: %w", dateStr, err)
		}
		target = parsed
	} else {
		target = now.In(msk)
	}
	endMSK := time.Date(target.Year(), target.Month(), target.Day(), domain.DailyRunHourMSK, 0, 0, 0, msk)
	window, err := domain.NewTimeWindow(endMSK.Add(-domain.NewsWindow).UTC(), endMSK.UTC())
	if err != nil {
		return time.Time{}, domain.TimeWindow{}, err
	}
	snapDate := time.Date(target.Year(), target.Month(), target.Day(), 0, 0, 0, 0, time.UTC)
	return snapDate, window, nil
}

// DailyNewsSnapshotWorkflow creates an immutable 24-hour snapshot of collected
// articles for 09:00 Europe/Moscow without AI ranking.
func DailyNewsSnapshotWorkflow(ctx workflow.Context, in DailyNewsSnapshotInput) (DailySnapshotResult, error) {
	info := workflow.GetInfo(ctx)
	snapDate, window, err := ComputeMoscowSnapshotWindow(in.SnapshotDate, workflow.Now(ctx))
	if err != nil {
		return DailySnapshotResult{}, temporal.NewNonRetryableApplicationError(err.Error(), "PermanentError", err)
	}

	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    10 * time.Second,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"PermanentError",
			},
		},
	})

	var res DailySnapshotResult
	if err := workflow.ExecuteActivity(actCtx, "CreateDailySnapshot", CreateDailySnapshotInput{
		SnapshotDate:             snapDate,
		WindowStart:              window.Start,
		WindowEnd:                window.End,
		WorkflowID:               info.WorkflowExecution.ID,
		IncludeSubmittedFallback: in.IncludeSubmittedFallback,
	}).Get(ctx, &res); err != nil {
		return DailySnapshotResult{}, err
	}
	return res, nil
}
