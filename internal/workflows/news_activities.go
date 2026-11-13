package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
)

// NewsActivities implements Temporal activities for news ingestion and daily
// snapshot generation.
type NewsActivities struct {
	Repo       *postgres.NewsRepository
	HTTPClient *news.HTTPClient
	NewsConfig config.NewsConfig
	Sources    []news.SourceConfig
	Logger     *slog.Logger
	Metrics    *observability.Metrics
}

// ListEnabledSourcesInput configures source discovery for an ingestion run.
type ListEnabledSourcesInput struct {
	SourceID  string   `json:"source_id,omitempty"`
	SourceIDs []string `json:"source_ids,omitempty"`
}

// FetchSourceInput configures fetching a single news source.
type FetchSourceInput struct {
	RunID  string            `json:"run_id"`
	Source news.SourceConfig `json:"source"`
	Limit  int               `json:"limit,omitempty"`
	DryRun bool              `json:"dry_run,omitempty"`
	Now    time.Time         `json:"now"`
}

// SourceIngestionResult summarizes the outcome of ingesting one source.
type SourceIngestionResult struct {
	SourceID       string `json:"source_id"`
	SourceName     string `json:"source_name"`
	Status         string `json:"status"` // SUCCESS, NOT_MODIFIED, ERROR
	FetchedCount   int    `json:"fetched_count"`
	InsertedCount  int    `json:"inserted_count"`
	DuplicateCount int    `json:"duplicate_count"`
	ErrorCount     int    `json:"error_count"`
	Error          string `json:"error,omitempty"`
	DurationMS     int64  `json:"duration_ms"`
}

// CreateDailySnapshotInput configures creating a 24h snapshot.
type CreateDailySnapshotInput struct {
	SnapshotDate             time.Time `json:"snapshot_date"`
	WindowStart              time.Time `json:"window_start"`
	WindowEnd                time.Time `json:"window_end"`
	WorkflowID               string    `json:"workflow_id"`
	IncludeSubmittedFallback bool      `json:"include_submitted_fallback"`
}

// DailySnapshotResult is the structured output of DailyNewsSnapshotWorkflow.
type DailySnapshotResult struct {
	SnapshotID     string         `json:"snapshot_id"`
	SnapshotDate   string         `json:"snapshot_date"`
	WindowStart    time.Time      `json:"window_start"`
	WindowEnd      time.Time      `json:"window_end"`
	CoverageStatus string         `json:"coverage_status"`
	ArticleCount   int            `json:"article_count"`
	SourceStats    map[string]int `json:"source_stats"`
	ArticleIDs     []string       `json:"article_ids"`
}

// ListEnabledSources ensures configured sources are registered in PostgreSQL
// and returns the enabled sources matching the optional filter.
func (a *NewsActivities) ListEnabledSources(ctx context.Context, in ListEnabledSourcesInput) ([]news.SourceConfig, error) {
	if len(a.Sources) > 0 {
		if err := a.Repo.EnsureSources(ctx, a.Sources); err != nil {
			return nil, classifyPGError(err)
		}
	}

	all, err := a.Repo.ListSources(ctx)
	if err != nil {
		return nil, classifyPGError(err)
	}

	filterID := strings.TrimSpace(in.SourceID)
	allowSet := make(map[string]struct{}, len(in.SourceIDs))
	for _, id := range in.SourceIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			allowSet[trimmed] = struct{}{}
		}
	}
	var enabled []news.SourceConfig
	foundFilter := false
	for _, item := range all {
		if filterID != "" && item.Config.ID != filterID {
			continue
		}
		if len(allowSet) > 0 {
			if _, ok := allowSet[item.Config.ID]; !ok {
				continue
			}
		}
		foundFilter = true
		if item.Config.Enabled {
			// Preserve per-source limit from static config if present.
			for _, def := range a.Sources {
				if def.ID == item.Config.ID && def.Limit > 0 {
					item.Config.Limit = def.Limit
					break
				}
			}
			enabled = append(enabled, item.Config)
		}
	}

	if filterID != "" && !foundFilter {
		return nil, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("unknown source %q", filterID),
			"UnknownSource",
			nil,
		)
	}
	return enabled, nil
}

// FetchAndPersistSource fetches one source via HTTP, normalizes and deduplicates
// articles, persists them to PostgreSQL (unless dry-run), and updates fetch state.
func (a *NewsActivities) FetchAndPersistSource(ctx context.Context, in FetchSourceInput) (SourceIngestionResult, error) {
	start := time.Now()
	info := activity.GetInfo(ctx)
	wfID := info.WorkflowExecution.ID
	sourceID := in.Source.ID

	res := SourceIngestionResult{
		SourceID:   sourceID,
		SourceName: in.Source.Name,
		Status:     "SUCCESS",
	}

	src, err := news.BuildSource(in.Source, a.HTTPClient, a.NewsConfig)
	if err != nil {
		res.Status = "ERROR"
		res.ErrorCount = 1
		res.Error = err.Error()
		res.DurationMS = time.Since(start).Milliseconds()
		a.recordMetricsAndLog(sourceID, wfID, in.RunID, res, "config_error")
		return res, nil
	}

	state, err := a.Repo.GetSourceFetchState(ctx, sourceID)
	if err != nil {
		return res, classifyPGError(err)
	}

	now := in.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	fetchRes, fetchErr := src.Fetch(ctx, news.FetchOptions{
		ETag:         state.ETag,
		LastModified: state.LastModified,
		Limit:        in.Limit,
		Now:          now,
	})
	if fetchErr != nil {
		res.Status = "ERROR"
		res.ErrorCount = 1
		res.Error = fetchErr.Error()
		res.DurationMS = time.Since(start).Milliseconds()

		errType := classifyFetchErrorType(fetchErr)
		if !in.DryRun {
			state.LastFetchAt = &now
			state.LastError = fetchErr.Error()
			state.ConsecutiveFailures++
			_ = a.Repo.UpdateSourceFetchState(ctx, state)
			_ = a.Repo.RecordSourceFetchHistory(ctx, postgres.SourceFetchHistory{
				SourceID:     sourceID,
				WorkflowID:   wfID,
				Status:       "ERROR",
				ErrorMessage: fetchErr.Error(),
				DurationMS:   res.DurationMS,
			})
		}

		a.recordMetricsAndLog(sourceID, wfID, in.RunID, res, errType)
		if isRetryableFetchError(fetchErr) {
			return res, fetchErr
		}
		// Permanent source errors (invalid RSS, 404, SSRF) are recorded and
		// returned cleanly so other sources continue unaffected.
		return res, nil
	}

	if fetchRes.NotModified {
		res.Status = "NOT_MODIFIED"
		res.DurationMS = time.Since(start).Milliseconds()
		if !in.DryRun {
			state.LastFetchAt = &now
			state.LastSuccessAt = &now
			state.LastError = ""
			state.ConsecutiveFailures = 0
			if err := a.Repo.UpdateSourceFetchState(ctx, state); err != nil {
				return res, classifyPGError(err)
			}
			_ = a.Repo.RecordSourceFetchHistory(ctx, postgres.SourceFetchHistory{
				SourceID:   sourceID,
				WorkflowID: wfID,
				Status:     "NOT_MODIFIED",
				DurationMS: res.DurationMS,
			})
		}
		a.recordMetricsAndLog(sourceID, wfID, in.RunID, res, "")
		return res, nil
	}

	deduped, batchDupes := news.DeduplicateArticles(fetchRes.Articles)
	res.FetchedCount = len(fetchRes.Articles)
	res.DuplicateCount += batchDupes

	if in.DryRun {
		res.InsertedCount = len(deduped)
	} else {
		for _, art := range deduped {
			_, inserted, err := a.Repo.UpsertArticle(ctx, art)
			if err != nil {
				res.ErrorCount++
				if res.Error == "" {
					res.Error = err.Error()
				}
				continue
			}
			if inserted {
				res.InsertedCount++
			} else {
				res.DuplicateCount++
			}
		}

		state.ETag = fetchRes.ETag
		state.LastModified = fetchRes.LastModified
		state.LastFetchAt = &now
		state.LastSuccessAt = &now
		state.LastError = ""
		state.ConsecutiveFailures = 0
		if err := a.Repo.UpdateSourceFetchState(ctx, state); err != nil {
			return res, classifyPGError(err)
		}
		_ = a.Repo.RecordSourceFetchHistory(ctx, postgres.SourceFetchHistory{
			SourceID:       sourceID,
			WorkflowID:     wfID,
			Status:         "SUCCESS",
			FetchedCount:   res.FetchedCount,
			InsertedCount:  res.InsertedCount,
			DuplicateCount: res.DuplicateCount,
			ErrorMessage:   res.Error,
			DurationMS:     time.Since(start).Milliseconds(),
		})
	}

	res.DurationMS = time.Since(start).Milliseconds()
	a.recordMetricsAndLog(sourceID, wfID, in.RunID, res, "")
	return res, nil
}

// CreateDailySnapshot queries articles in [WindowStart, WindowEnd), checks
// coverage completeness, and persists the snapshot in PostgreSQL.
func (a *NewsActivities) CreateDailySnapshot(ctx context.Context, in CreateDailySnapshotInput) (DailySnapshotResult, error) {
	window, err := domain.NewTimeWindow(in.WindowStart.UTC(), in.WindowEnd.UTC())
	if err != nil {
		return DailySnapshotResult{}, permanentError(err)
	}

	articles, err := a.Repo.GetArticlesByTimeRange(ctx, window.Start, window.End, in.IncludeSubmittedFallback)
	if err != nil {
		return DailySnapshotResult{}, classifyPGError(err)
	}

	earliest, err := a.Repo.GetEarliestFetchTime(ctx)
	if err != nil {
		return DailySnapshotResult{}, classifyPGError(err)
	}
	coverage := "PARTIAL_COVERAGE"
	if earliest != nil && !earliest.After(window.Start) {
		coverage = "FULL_COVERAGE"
	}

	sourceStats := make(map[string]int)
	articleIDs := make([]string, 0, len(articles))
	for _, art := range articles {
		articleIDs = append(articleIDs, art.ID.String())
		sourceStats[art.SourceID]++
	}

	snap := postgres.DailySnapshot{
		SnapshotDate:   in.SnapshotDate.UTC(),
		Window:         window,
		CoverageStatus: coverage,
		SourceStats:    sourceStats,
		WorkflowID:     in.WorkflowID,
	}
	persisted, err := a.Repo.CreateDailySnapshot(ctx, snap, articleIDs)
	if err != nil {
		return DailySnapshotResult{}, classifyPGError(err)
	}

	return DailySnapshotResult{
		SnapshotID:     persisted.ID,
		SnapshotDate:   in.SnapshotDate.Format("2006-01-02"),
		WindowStart:    window.Start,
		WindowEnd:      window.End,
		CoverageStatus: persisted.CoverageStatus,
		ArticleCount:   persisted.ArticleCount,
		SourceStats:    sourceStats,
		ArticleIDs:     articleIDs,
	}, nil
}

func (a *NewsActivities) recordMetricsAndLog(sourceID, workflowID, runID string, res SourceIngestionResult, errType string) {
	if a.Metrics != nil {
		status := strings.ToLower(res.Status)
		a.Metrics.NewsFetchTotal.WithLabelValues(sourceID, status).Inc()
		a.Metrics.NewsFetchDurationSeconds.WithLabelValues(sourceID, status).Observe(float64(res.DurationMS) / 1000.0)
		if res.ErrorCount > 0 {
			if errType == "" {
				errType = "persist_error"
			}
			a.Metrics.NewsFetchErrorsTotal.WithLabelValues(sourceID, errType).Add(float64(res.ErrorCount))
		}
		if res.FetchedCount > 0 {
			a.Metrics.NewsArticlesFetchedTotal.WithLabelValues(sourceID).Add(float64(res.FetchedCount))
		}
		if res.InsertedCount > 0 {
			a.Metrics.NewsArticlesInsertedTotal.WithLabelValues(sourceID).Add(float64(res.InsertedCount))
		}
		if res.DuplicateCount > 0 {
			a.Metrics.NewsDuplicatesTotal.WithLabelValues(sourceID).Add(float64(res.DuplicateCount))
		}
		if res.Status == "SUCCESS" || res.Status == "NOT_MODIFIED" {
			a.Metrics.NewsSourceLastSuccessTS.WithLabelValues(sourceID).SetToCurrentTime()
		}
	}

	if a.Logger != nil {
		attrs := []any{
			"source_id", sourceID,
			"workflow_id", workflowID,
			"run_id", runID,
			"fetched_count", res.FetchedCount,
			"inserted_count", res.InsertedCount,
			"duplicate_count", res.DuplicateCount,
			"error_count", res.ErrorCount,
			"duration_ms", res.DurationMS,
			"status", res.Status,
		}
		if res.Error != "" {
			attrs = append(attrs, "error", res.Error)
			a.Logger.Warn("news source ingestion finished with error", attrs...)
			return
		}
		a.Logger.Info("news source ingestion finished", attrs...)
	}
}

func isRetryableFetchError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, news.ErrInvalidScheme) ||
		errors.Is(err, news.ErrSSRFBlocked) ||
		errors.Is(err, news.ErrOversizedResponse) ||
		errors.Is(err, news.ErrTooManyRedirects) {
		return false
	}
	var upstream *news.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Retryable()
	}
	// XML parse / invalid RSS errors are permanent for the current feed body.
	if strings.Contains(err.Error(), "parse feed") || strings.Contains(err.Error(), "XML syntax error") {
		return false
	}
	return true
}

func classifyFetchErrorType(err error) string {
	if err == nil {
		return ""
	}
	var upstream *news.UpstreamError
	if errors.As(err, &upstream) {
		return fmt.Sprintf("http_%d", upstream.StatusCode)
	}
	if errors.Is(err, news.ErrSSRFBlocked) {
		return "ssrf_blocked"
	}
	if errors.Is(err, news.ErrOversizedResponse) {
		return "oversized_response"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "fetch_error"
}
