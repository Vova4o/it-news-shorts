package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/it-news-shorts/it-news-shorts/internal/editor"
)

// DailyEditorialWorkflowName is the Temporal workflow name for daily AI editing.
const DailyEditorialWorkflowName = "DailyEditorialWorkflow"

// DailyEditorialInput configures a DailyEditorialWorkflow execution.
type DailyEditorialInput struct {
	Date   time.Time `json:"date"`
	DryRun bool      `json:"dry_run,omitempty"`
}

// DailyEditorialWorkflow orchestrates snapshot loading, preprocessing,
// clustering, claim extraction, fact verification, scoring, TOP-3 selection,
// PostgreSQL persistence, and editorial report generation.
func DailyEditorialWorkflow(ctx workflow.Context, in DailyEditorialInput) (editor.EditorialReport, error) {
	startedAt := workflow.Now(ctx).UTC()
	targetDate := in.Date.UTC()
	if targetDate.IsZero() {
		targetDate = startedAt
	}

	wfID := workflow.GetInfo(ctx).WorkflowExecution.ID
	runID := fmt.Sprintf("edrun-%s-%s", targetDate.Format("2006-01-02"), wfID)

	ioOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 3 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"CONFIG_ERROR",
				"LLM_CONFIG_ERROR",
				"PROMPT_CONFIG_ERROR",
				"BUDGET_EXCEEDED",
			},
		},
	}
	llmOpts := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    3 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"CONFIG_ERROR",
				"LLM_CONFIG_ERROR",
				"PROMPT_CONFIG_ERROR",
				"BUDGET_EXCEEDED",
			},
		},
	}

	ioCtx := workflow.WithActivityOptions(ctx, ioOpts)
	llmCtx := workflow.WithActivityOptions(ctx, llmOpts)

	var acts *EditorialActivities
	var allUsage []editor.LLMUsageRecord

	var snapOut LoadDailySnapshotOutput
	if err := workflow.ExecuteActivity(ioCtx, acts.LoadDailySnapshot, LoadDailySnapshotInput{
		RunID:  runID,
		Date:   targetDate,
		DryRun: in.DryRun,
	}).Get(ctx, &snapOut); err != nil {
		return editor.EditorialReport{}, err
	}

	var prepOut PreprocessArticlesOutput
	if err := workflow.ExecuteActivity(ioCtx, acts.PreprocessArticles, PreprocessArticlesInput{
		Articles:    snapOut.Articles,
		WindowStart: snapOut.WindowStart,
		WindowEnd:   snapOut.WindowEnd,
	}).Get(ctx, &prepOut); err != nil {
		return editor.EditorialReport{}, err
	}

	var clusterOut ClusterStoriesOutput
	if err := workflow.ExecuteActivity(llmCtx, acts.ClusterStories, ClusterStoriesInput{
		RunID:    runID,
		Articles: prepOut.Articles,
		Now:      startedAt,
	}).Get(ctx, &clusterOut); err != nil {
		return editor.EditorialReport{}, err
	}
	allUsage = append(allUsage, clusterOut.Usage...)

	var extractOut ExtractClaimsOutput
	if err := workflow.ExecuteActivity(llmCtx, acts.ExtractClaims, ExtractClaimsInput{
		RunID:   runID,
		Stories: clusterOut.Stories,
	}).Get(ctx, &extractOut); err != nil {
		return editor.EditorialReport{}, err
	}
	allUsage = append(allUsage, extractOut.Usage...)

	var verifyOut VerifyClaimsOutput
	if err := workflow.ExecuteActivity(llmCtx, acts.VerifyClaims, VerifyClaimsInput{
		RunID:   runID,
		Stories: extractOut.Stories,
		Now:     startedAt,
	}).Get(ctx, &verifyOut); err != nil {
		return editor.EditorialReport{}, err
	}
	allUsage = append(allUsage, verifyOut.Usage...)

	var filterOut FilterEligibleStoriesOutput
	if err := workflow.ExecuteActivity(ioCtx, acts.FilterEligibleStories, FilterEligibleStoriesInput{
		Stories: verifyOut.Stories,
	}).Get(ctx, &filterOut); err != nil {
		return editor.EditorialReport{}, err
	}

	var scoreOut ScoreStoriesOutput
	if len(filterOut.Eligible) > 0 {
		if err := workflow.ExecuteActivity(llmCtx, acts.ScoreStories, ScoreStoriesInput{
			RunID:   runID,
			Stories: filterOut.Eligible,
			Now:     startedAt,
		}).Get(ctx, &scoreOut); err != nil {
			return editor.EditorialReport{}, err
		}
		allUsage = append(allUsage, scoreOut.Usage...)
	}

	var selOut editor.SelectionResult
	if err := workflow.ExecuteActivity(ioCtx, acts.SelectTopStories, SelectTopStoriesInput{
		Candidates:           scoreOut.Stories,
		PreviousFingerprints: snapOut.PreviousFingerprints,
	}).Get(ctx, &selOut); err != nil {
		return editor.EditorialReport{}, err
	}

	allStories := make([]editor.ClusteredStory, 0, len(scoreOut.Stories)+len(filterOut.Rejected))
	allStories = append(allStories, scoreOut.Stories...)
	allStories = append(allStories, filterOut.Rejected...)

	durationMS := workflow.Now(ctx).UTC().Sub(startedAt).Milliseconds()
	var report editor.EditorialReport
	if err := workflow.ExecuteActivity(ioCtx, acts.GenerateEditorialReport, GenerateEditorialReportInput{
		Date:              targetDate,
		RunID:             runID,
		SnapshotID:        snapOut.SnapshotID,
		WorkflowID:        wfID,
		DryRun:            in.DryRun,
		ArticlesProcessed: len(prepOut.Articles),
		TotalStories:      len(clusterOut.Stories),
		VerifiedStories:   len(filterOut.Eligible),
		RejectedStories:   len(filterOut.Rejected),
		Selection:         selOut,
		Usage:             allUsage,
		DurationMS:        durationMS,
	}).Get(ctx, &report); err != nil {
		return editor.EditorialReport{}, err
	}

	if err := workflow.ExecuteActivity(ioCtx, acts.PersistEditorialResults, PersistEditorialResultsInput{
		RunID:      runID,
		SnapshotID: snapOut.SnapshotID,
		RunDate:    targetDate,
		DryRun:     in.DryRun,
		Stories:    allStories,
		Selection:  selOut,
		Report:     report,
		Usage:      allUsage,
	}).Get(ctx, nil); err != nil {
		return editor.EditorialReport{}, err
	}

	return report, nil
}
