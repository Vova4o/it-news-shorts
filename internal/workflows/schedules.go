package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
)

const (
	// NewsIngestionScheduleID is the Temporal schedule ID for continuous ingestion.
	NewsIngestionScheduleID = "news-ingestion-schedule"
	// DailyNewsSnapshotScheduleID is the Temporal schedule ID for 09:00 MSK snapshots.
	DailyNewsSnapshotScheduleID = "daily-news-snapshot-schedule"
)

// EnsureNewsSchedules idempotently creates or updates the Temporal schedules
// for continuous news ingestion and daily 09:00 MSK snapshots.
func EnsureNewsSchedules(
	ctx context.Context,
	tc client.Client,
	taskQueue string,
	cfg config.NewsConfig,
	logger *slog.Logger,
) error {
	if !cfg.SchedulesEnabled {
		if logger != nil {
			logger.Info("news temporal schedules disabled by configuration")
		}
		return nil
	}
	interval := cfg.IngestionInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}

	sc := tc.ScheduleClient()

	if err := ensureSchedule(ctx, sc, client.ScheduleOptions{
		ID: NewsIngestionScheduleID,
		Spec: client.ScheduleSpec{
			Intervals: []client.ScheduleIntervalSpec{
				{Every: interval},
			},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "news-ingestion-scheduled",
			Workflow:  NewsIngestionWorkflowName,
			TaskQueue: taskQueue,
			Args: []any{NewsIngestionInput{
				MaxConcurrency: cfg.MaxConcurrency,
			}},
		},
		Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
	}); err != nil {
		return fmt.Errorf("workflows: ensure %s: %w", NewsIngestionScheduleID, err)
	}

	// 09:00 Europe/Moscow == 06:00 UTC (fixed UTC+3 since 2014).
	if err := ensureSchedule(ctx, sc, client.ScheduleOptions{
		ID: DailyNewsSnapshotScheduleID,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{"0 6 * * *"},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "daily-news-snapshot-scheduled",
			Workflow:  DailyNewsSnapshotWorkflowName,
			TaskQueue: taskQueue,
			Args: []any{DailyNewsSnapshotInput{
				IncludeSubmittedFallback: true,
			}},
		},
		Overlap: enums.SCHEDULE_OVERLAP_POLICY_SKIP,
	}); err != nil {
		return fmt.Errorf("workflows: ensure %s: %w", DailyNewsSnapshotScheduleID, err)
	}

	if logger != nil {
		logger.Info("news temporal schedules ensured",
			"ingestion_schedule_id", NewsIngestionScheduleID,
			"ingestion_interval", interval.String(),
			"snapshot_schedule_id", DailyNewsSnapshotScheduleID,
		)
	}
	return nil
}

func ensureSchedule(ctx context.Context, sc client.ScheduleClient, opts client.ScheduleOptions) error {
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	handle := sc.GetHandle(opCtx, opts.ID)
	_, err := handle.Describe(opCtx)
	if err == nil {
		return handle.Update(opCtx, client.ScheduleUpdateOptions{
			DoUpdate: func(in client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
				sched := in.Description.Schedule
				sched.Spec = &opts.Spec
				sched.Action = opts.Action
				if sched.Policy == nil {
					sched.Policy = &client.SchedulePolicies{}
				}
				sched.Policy.Overlap = opts.Overlap
				return &client.ScheduleUpdate{
					Schedule: &sched,
				}, nil
			},
		})
	}

	var notFound *serviceerror.NotFound
	if !errors.As(err, &notFound) {
		return err
	}

	_, createErr := sc.Create(opCtx, opts)
	var alreadyExists *serviceerror.AlreadyExists
	if createErr != nil && !errors.As(createErr, &alreadyExists) {
		return createErr
	}
	return nil
}
