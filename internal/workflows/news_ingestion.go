package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

// NewsIngestionWorkflowName is the registered Temporal workflow name.
const NewsIngestionWorkflowName = "NewsIngestionWorkflow"

// NewsIngestionInput configures one news ingestion workflow execution.
type NewsIngestionInput struct {
	RunID          string   `json:"run_id"`
	SourceID       string   `json:"source_id,omitempty"`
	SourceIDs      []string `json:"source_ids,omitempty"`
	Limit          int      `json:"limit,omitempty"`
	DryRun         bool     `json:"dry_run,omitempty"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
}

// NewsIngestionResult summarizes the outcome of NewsIngestionWorkflow.
type NewsIngestionResult struct {
	RunID            string                  `json:"run_id"`
	WorkflowID       string                  `json:"workflow_id"`
	Status           string                  `json:"status"` // COMPLETED, PARTIAL_FAILURE, FAILED
	SourcesTotal     int                     `json:"sources_total"`
	SourcesSucceeded int                     `json:"sources_succeeded"`
	SourcesFailed    int                     `json:"sources_failed"`
	FetchedCount     int                     `json:"fetched_count"`
	InsertedCount    int                     `json:"inserted_count"`
	DuplicateCount   int                     `json:"duplicate_count"`
	DurationMS       int64                   `json:"duration_ms"`
	Results          []SourceIngestionResult `json:"results"`
}

// NewsIngestionWorkflow discovers enabled sources, fetches them in parallel
// with bounded concurrency, deduplicates, and persists articles to PostgreSQL.
func NewsIngestionWorkflow(ctx workflow.Context, in NewsIngestionInput) (NewsIngestionResult, error) {
	info := workflow.GetInfo(ctx)
	start := workflow.Now(ctx).UTC()
	logger := workflow.GetLogger(ctx)

	runID := in.RunID
	if runID == "" {
		runID = info.WorkflowExecution.ID
	}

	actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    500 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    10 * time.Second,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"PermanentError",
				"UnknownSource",
			},
		},
	})

	var sources []news.SourceConfig
	if err := workflow.ExecuteActivity(actCtx, "ListEnabledSources", ListEnabledSourcesInput{
		SourceID:  in.SourceID,
		SourceIDs: in.SourceIDs,
	}).Get(ctx, &sources); err != nil {
		return NewsIngestionResult{}, err
	}

	maxConc := in.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 4
	}

	out := NewsIngestionResult{
		RunID:        runID,
		WorkflowID:   info.WorkflowExecution.ID,
		SourcesTotal: len(sources),
		Results:      make([]SourceIngestionResult, 0, len(sources)),
	}

	for i := 0; i < len(sources); i += maxConc {
		end := i + maxConc
		if end > len(sources) {
			end = len(sources)
		}
		batch := sources[i:end]
		futures := make([]workflow.Future, len(batch))
		for j, src := range batch {
			futures[j] = workflow.ExecuteActivity(actCtx, "FetchAndPersistSource", FetchSourceInput{
				RunID:  runID,
				Source: src,
				Limit:  in.Limit,
				DryRun: in.DryRun,
				Now:    start,
			})
		}

		for j, fut := range futures {
			var res SourceIngestionResult
			if err := fut.Get(ctx, &res); err != nil {
				res = SourceIngestionResult{
					SourceID:   batch[j].ID,
					SourceName: batch[j].Name,
					Status:     "ERROR",
					ErrorCount: 1,
					Error:      err.Error(),
				}
			}
			out.Results = append(out.Results, res)
			out.FetchedCount += res.FetchedCount
			out.InsertedCount += res.InsertedCount
			out.DuplicateCount += res.DuplicateCount
			if res.Status == "ERROR" {
				out.SourcesFailed++
			} else {
				out.SourcesSucceeded++
			}
		}
	}

	out.DurationMS = workflow.Now(ctx).UTC().Sub(start).Milliseconds()
	switch {
	case out.SourcesTotal > 0 && out.SourcesSucceeded == 0:
		out.Status = "FAILED"
	case out.SourcesFailed > 0:
		out.Status = "PARTIAL_FAILURE"
	default:
		out.Status = "COMPLETED"
	}

	logger.Info("NewsIngestionWorkflow finished",
		"workflow_id", out.WorkflowID,
		"run_id", out.RunID,
		"status", out.Status,
		"sources_total", out.SourcesTotal,
		"sources_succeeded", out.SourcesSucceeded,
		"sources_failed", out.SourcesFailed,
		"fetched_count", out.FetchedCount,
		"inserted_count", out.InsertedCount,
		"duplicate_count", out.DuplicateCount,
	)

	if out.Status == "FAILED" {
		return out, fmt.Errorf("workflows: all %d news sources failed", out.SourcesTotal)
	}
	return out, nil
}
