package workflows

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"go.temporal.io/sdk/temporal"

	"github.com/it-news-shorts/it-news-shorts/internal/ai"
	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
)

// EditorialActivities implements Temporal activities for DailyEditorialWorkflow.
type EditorialActivities struct {
	NewsRepo     *postgres.NewsRepository
	EditorRepo   *postgres.EditorRepository
	LLM          ai.LLMProvider
	Prompts      *ai.PromptRegistry
	Budget       *ai.BudgetTracker
	LLMConfig    config.LLMConfig
	EditorConfig config.EditorConfig
	Logger       *slog.Logger
	Metrics      *observability.Metrics
}

// LoadDailySnapshotInput requests snapshot loading for a target date.
type LoadDailySnapshotInput struct {
	RunID  string    `json:"run_id"`
	Date   time.Time `json:"date"`
	DryRun bool      `json:"dry_run,omitempty"`
}

// LoadDailySnapshotOutput contains the snapshot metadata, articles, and recent
// published fingerprints for repeat protection.
type LoadDailySnapshotOutput struct {
	SnapshotID           string                `json:"snapshot_id"`
	WindowStart          time.Time             `json:"window_start"`
	WindowEnd            time.Time             `json:"window_end"`
	CoverageStatus       string                `json:"coverage_status"`
	Articles             []news.UnifiedArticle `json:"articles"`
	PreviousFingerprints map[string]bool       `json:"previous_fingerprints"`
}

// LoadDailySnapshot loads the persisted daily snapshot and its articles from PostgreSQL.
func (a *EditorialActivities) LoadDailySnapshot(ctx context.Context, in LoadDailySnapshotInput) (LoadDailySnapshotOutput, error) {
	if a.NewsRepo == nil {
		return LoadDailySnapshotOutput{}, temporal.NewNonRetryableApplicationError("news repository is nil", "CONFIG_ERROR", nil)
	}
	targetDate := in.Date.UTC()
	if targetDate.IsZero() {
		targetDate = time.Now().UTC()
	}

	snap, err := a.NewsRepo.GetDailySnapshot(ctx, targetDate)
	if err != nil {
		return LoadDailySnapshotOutput{}, fmt.Errorf("load daily snapshot for %s: %w", targetDate.Format("2006-01-02"), err)
	}

	articles, err := a.NewsRepo.GetArticlesByTimeRange(ctx, snap.Window.Start, snap.Window.End, true)
	if err != nil {
		return LoadDailySnapshotOutput{}, fmt.Errorf("load articles for snapshot %s: %w", snap.ID, err)
	}

	prevFP := make(map[string]bool)
	if a.EditorRepo != nil {
		if fp, err := a.EditorRepo.GetPreviouslyPublishedFingerprints(ctx, targetDate, 7); err == nil {
			prevFP = fp
		}
		if !in.DryRun && in.RunID != "" {
			if _, err := a.EditorRepo.CreateOrGetEditorialRun(ctx, postgres.EditorialRunInput{
				ID:            in.RunID,
				SnapshotID:    snap.ID,
				RunDate:       targetDate,
				Status:        "RUNNING",
				Model:         a.LLMConfig.Model,
				PromptVersion: "v1",
			}); err != nil {
				return LoadDailySnapshotOutput{}, fmt.Errorf("init editorial run %s: %w", in.RunID, err)
			}
		}
	}

	return LoadDailySnapshotOutput{
		SnapshotID:           snap.ID,
		WindowStart:          snap.Window.Start,
		WindowEnd:            snap.Window.End,
		CoverageStatus:       snap.CoverageStatus,
		Articles:             articles,
		PreviousFingerprints: prevFP,
	}, nil
}

// PreprocessArticlesInput provides raw snapshot articles for normalization and capping.
type PreprocessArticlesInput struct {
	Articles    []news.UnifiedArticle `json:"articles"`
	WindowStart time.Time             `json:"window_start"`
	WindowEnd   time.Time             `json:"window_end"`
	MaxArticles int                   `json:"max_articles"`
}

// PreprocessArticlesOutput returns clean, deduplicated, window-filtered articles.
type PreprocessArticlesOutput struct {
	Articles []news.UnifiedArticle `json:"articles"`
	Dropped  int                   `json:"dropped"`
}

// PreprocessArticles filters articles by time window, removes empty titles and
// duplicate canonical URLs, and caps at MaxArticlesPerRun.
func (a *EditorialActivities) PreprocessArticles(_ context.Context, in PreprocessArticlesInput) (PreprocessArticlesOutput, error) {
	maxArt := in.MaxArticles
	if maxArt <= 0 {
		maxArt = 100
	}

	seenURL := make(map[string]bool, len(in.Articles))
	var clean []news.UnifiedArticle
	dropped := 0

	for _, art := range in.Articles {
		if strings.TrimSpace(art.Title) == "" {
			dropped++
			continue
		}
		refTime := art.DiscoveredAt
		if art.PublishedAt != nil && !art.PublishedAt.IsZero() {
			refTime = *art.PublishedAt
		}
		if !in.WindowStart.IsZero() && refTime.Before(in.WindowStart) {
			dropped++
			continue
		}
		if !in.WindowEnd.IsZero() && refTime.After(in.WindowEnd.Add(5*time.Minute)) {
			dropped++
			continue
		}
		key := strings.ToLower(strings.TrimSpace(art.CanonicalURL))
		if key != "" {
			if seenURL[key] {
				dropped++
				continue
			}
			seenURL[key] = true
		}
		clean = append(clean, art)
	}

	sort.Slice(clean, func(i, j int) bool {
		si, sj := 0, 0
		if clean[i].PopularityScore != nil {
			si = *clean[i].PopularityScore
		}
		if clean[j].PopularityScore != nil {
			sj = *clean[j].PopularityScore
		}
		if si != sj {
			return si > sj
		}
		return clean[i].FetchedAt.After(clean[j].FetchedAt)
	})

	if len(clean) > maxArt {
		dropped += len(clean) - maxArt
		clean = clean[:maxArt]
	}

	return PreprocessArticlesOutput{Articles: clean, Dropped: dropped}, nil
}

// ClusterStoriesInput groups preprocessed articles for clustering.
type ClusterStoriesInput struct {
	RunID    string                `json:"run_id"`
	Articles []news.UnifiedArticle `json:"articles"`
	Now      time.Time             `json:"now"`
}

// ClusterStoriesOutput returns clustered stories and LLM usage telemetry.
type ClusterStoriesOutput struct {
	Stories []editor.ClusteredStory `json:"stories"`
	Usage   []editor.LLMUsageRecord `json:"usage"`
}

// ClusterStories groups articles about the same real-world event.
func (a *EditorialActivities) ClusterStories(ctx context.Context, in ClusterStoriesInput) (ClusterStoriesOutput, error) {
	if err := a.requireLLM(); err != nil {
		return ClusterStoriesOutput{}, err
	}
	clusterer := editor.NewStoryClusterer(a.LLM, a.Prompts, a.LLMConfig.Model)
	res, err := clusterer.Cluster(ctx, in.Articles, in.Now)
	if err != nil {
		return ClusterStoriesOutput{}, fmt.Errorf("cluster stories: %w", err)
	}
	for i := range res.Usage {
		res.Usage[i].EditorialRunID = in.RunID
		a.recordUsageMetrics(res.Usage[i])
	}
	return ClusterStoriesOutput{Stories: res.Stories, Usage: res.Usage}, nil
}

// ExtractClaimsInput passes clustered stories for atomic claim extraction.
type ExtractClaimsInput struct {
	RunID   string                  `json:"run_id"`
	Stories []editor.ClusteredStory `json:"stories"`
}

// ExtractClaimsOutput returns stories populated with extracted claims.
type ExtractClaimsOutput struct {
	Stories []editor.ClusteredStory `json:"stories"`
	Usage   []editor.LLMUsageRecord `json:"usage"`
}

// ExtractClaims extracts atomic factual claims from each clustered story.
func (a *EditorialActivities) ExtractClaims(ctx context.Context, in ExtractClaimsInput) (ExtractClaimsOutput, error) {
	if err := a.requireLLM(); err != nil {
		return ExtractClaimsOutput{}, err
	}
	extractor := editor.NewFactExtractor(a.LLM, a.Prompts, a.LLMConfig.Model)
	out := make([]editor.ClusteredStory, 0, len(in.Stories))
	var usage []editor.LLMUsageRecord

	for _, st := range in.Stories {
		claims, rec, err := extractor.Extract(ctx, st)
		if err != nil {
			st.RejectionReason = fmt.Sprintf("claim extraction failed: %v", err)
			st.VerificationPassed = false
			out = append(out, st)
			continue
		}
		st.Claims = claims
		if rec != nil {
			rec.EditorialRunID = in.RunID
			a.recordUsageMetrics(*rec)
			usage = append(usage, *rec)
		}
		out = append(out, st)
	}
	return ExtractClaimsOutput{Stories: out, Usage: usage}, nil
}

// VerifyClaimsInput passes stories with claims for anti-hallucination verification.
type VerifyClaimsInput struct {
	RunID   string                  `json:"run_id"`
	Stories []editor.ClusteredStory `json:"stories"`
	Now     time.Time               `json:"now"`
}

// VerifyClaimsOutput returns stories with verified/rejected claim verdicts.
type VerifyClaimsOutput struct {
	Stories []editor.ClusteredStory `json:"stories"`
	Usage   []editor.LLMUsageRecord `json:"usage"`
}

// VerifyClaims runs deterministic + LLM fact verification on every story.
func (a *EditorialActivities) VerifyClaims(ctx context.Context, in VerifyClaimsInput) (VerifyClaimsOutput, error) {
	if err := a.requireLLM(); err != nil {
		return VerifyClaimsOutput{}, err
	}
	minClaims := a.EditorConfig.MinVerifiedClaims
	if minClaims <= 0 {
		minClaims = 2
	}
	checker := editor.NewFactCheckerEngine(a.LLM, a.Prompts, a.LLMConfig.Model, minClaims, nil)
	out := make([]editor.ClusteredStory, 0, len(in.Stories))
	var usage []editor.LLMUsageRecord

	for _, st := range in.Stories {
		if len(st.Claims) == 0 {
			st.VerificationPassed = false
			if st.RejectionReason == "" {
				st.RejectionReason = "no grounded factual claims extracted"
			}
			out = append(out, st)
			continue
		}
		checked, rec, err := checker.VerifyStory(ctx, st, in.Now)
		if err != nil {
			st.VerificationPassed = false
			st.RejectionReason = fmt.Sprintf("fact verification error: %v", err)
			out = append(out, st)
			continue
		}
		if rec != nil {
			rec.EditorialRunID = in.RunID
			a.recordUsageMetrics(*rec)
			usage = append(usage, *rec)
		}
		if a.Metrics != nil {
			for _, cl := range checked.Claims {
				a.Metrics.EditorClaimsVerified.WithLabelValues(string(cl.VerificationStatus)).Inc()
			}
		}
		out = append(out, checked)
	}
	return VerifyClaimsOutput{Stories: out, Usage: usage}, nil
}

// FilterEligibleStoriesInput groups verified + rejected stories.
type FilterEligibleStoriesInput struct {
	Stories []editor.ClusteredStory `json:"stories"`
}

// FilterEligibleStoriesOutput splits verified eligible stories from rejected ones.
type FilterEligibleStoriesOutput struct {
	Eligible []editor.ClusteredStory `json:"eligible"`
	Rejected []editor.ClusteredStory `json:"rejected"`
}

// FilterEligibleStories separates stories that passed fact-checking from rejected stories.
func (a *EditorialActivities) FilterEligibleStories(_ context.Context, in FilterEligibleStoriesInput) (FilterEligibleStoriesOutput, error) {
	var out FilterEligibleStoriesOutput
	for _, st := range in.Stories {
		if st.VerificationPassed {
			out.Eligible = append(out.Eligible, st)
		} else {
			out.Rejected = append(out.Rejected, st)
		}
	}
	if a.Metrics != nil {
		a.Metrics.EditorStoriesVerified.Add(float64(len(out.Eligible)))
		for _, r := range out.Rejected {
			reason := r.RejectionReason
			if reason == "" {
				reason = "unverified"
			}
			a.Metrics.EditorStoriesRejected.WithLabelValues(reason).Inc()
		}
	}
	return out, nil
}

// ScoreStoriesInput passes eligible verified stories for AI scoring.
type ScoreStoriesInput struct {
	RunID   string                  `json:"run_id"`
	Stories []editor.ClusteredStory `json:"stories"`
	Now     time.Time               `json:"now"`
}

// ScoreStoriesOutput returns scored stories and LLM telemetry.
type ScoreStoriesOutput struct {
	Stories []editor.ClusteredStory `json:"stories"`
	Usage   []editor.LLMUsageRecord `json:"usage"`
}

// ScoreStories evaluates Impact, Interest, Freshness, and Novelty and computes
// the deterministic weighted FinalScore.
func (a *EditorialActivities) ScoreStories(ctx context.Context, in ScoreStoriesInput) (ScoreStoriesOutput, error) {
	if err := a.requireLLM(); err != nil {
		return ScoreStoriesOutput{}, err
	}
	ranker := editor.NewStoryRankerEngine(a.LLM, a.Prompts, a.LLMConfig.Model, "v1")
	out := make([]editor.ClusteredStory, 0, len(in.Stories))
	var usage []editor.LLMUsageRecord

	for _, st := range in.Stories {
		scored, rec, err := ranker.ScoreStory(ctx, st, in.Now)
		if err != nil {
			scored.VerificationPassed = false
			scored.RejectionReason = fmt.Sprintf("scoring error: %v", err)
			out = append(out, scored)
			continue
		}
		if rec != nil {
			rec.EditorialRunID = in.RunID
			a.recordUsageMetrics(*rec)
			usage = append(usage, *rec)
		}
		out = append(out, scored)
	}
	return ScoreStoriesOutput{Stories: out, Usage: usage}, nil
}

// SelectTopStoriesInput passes scored candidates and repeat-protection fingerprints.
type SelectTopStoriesInput struct {
	Candidates           []editor.ClusteredStory `json:"candidates"`
	PreviousFingerprints map[string]bool         `json:"previous_fingerprints"`
	MaxStories           int                     `json:"max_stories"`
}

// SelectTopStories selects up to TOP-3 verified, diverse stories.
func (a *EditorialActivities) SelectTopStories(_ context.Context, in SelectTopStoriesInput) (editor.SelectionResult, error) {
	maxStories := in.MaxStories
	if maxStories <= 0 {
		maxStories = a.EditorConfig.MaxStories
	}
	if maxStories <= 0 {
		maxStories = 3
	}
	sel := editor.NewDailyStorySelector(maxStories)
	return sel.Select(in.Candidates, in.PreviousFingerprints), nil
}

// PersistEditorialResultsInput contains everything to save transactionally in PostgreSQL.
type PersistEditorialResultsInput struct {
	RunID      string                  `json:"run_id"`
	SnapshotID string                  `json:"snapshot_id"`
	RunDate    time.Time               `json:"run_date"`
	DryRun     bool                    `json:"dry_run,omitempty"`
	Stories    []editor.ClusteredStory `json:"stories"`
	Selection  editor.SelectionResult  `json:"selection"`
	Report     editor.EditorialReport  `json:"report"`
	Usage      []editor.LLMUsageRecord `json:"usage"`
}

// PersistEditorialResults saves stories, claims, evidence, scores, TOP-3 selection,
// and LLM usage into PostgreSQL unless DryRun is true.
func (a *EditorialActivities) PersistEditorialResults(ctx context.Context, in PersistEditorialResultsInput) error {
	if in.DryRun || a.EditorRepo == nil {
		return nil
	}
	return a.EditorRepo.SaveEditorialResults(ctx, postgres.SaveEditorialResultsParams{
		RunID:         in.RunID,
		SnapshotID:    in.SnapshotID,
		RunDate:       in.RunDate,
		Status:        "COMPLETED",
		Model:         a.LLMConfig.Model,
		PromptVersion: "v1",
		Stories:       in.Stories,
		Selection:     in.Selection,
		Report:        in.Report,
		Usage:         in.Usage,
	})
}

// GenerateEditorialReportInput builds the final EditorialReport.
type GenerateEditorialReportInput struct {
	Date              time.Time               `json:"date"`
	RunID             string                  `json:"run_id"`
	SnapshotID        string                  `json:"snapshot_id"`
	WorkflowID        string                  `json:"workflow_id"`
	DryRun            bool                    `json:"dry_run"`
	ArticlesProcessed int                     `json:"articles_processed"`
	TotalStories      int                     `json:"total_stories"`
	VerifiedStories   int                     `json:"verified_stories"`
	RejectedStories   int                     `json:"rejected_stories"`
	Selection         editor.SelectionResult  `json:"selection"`
	Usage             []editor.LLMUsageRecord `json:"usage"`
	DurationMS        int64                   `json:"duration_ms"`
}

// GenerateEditorialReport summarizes the editorial workflow execution.
func (a *EditorialActivities) GenerateEditorialReport(_ context.Context, in GenerateEditorialReportInput) (editor.EditorialReport, error) {
	status := "COMPLETED"
	if in.DryRun {
		status = "DRY_RUN"
	}
	var inTok, outTok int
	var cost float64
	for _, u := range in.Usage {
		inTok += u.InputTokens
		outTok += u.OutputTokens
		cost += u.CostUSD
	}
	return editor.EditorialReport{
		Date:              in.Date.UTC().Format("2006-01-02"),
		EditorialRunID:    in.RunID,
		SnapshotID:        in.SnapshotID,
		WorkflowID:        in.WorkflowID,
		Status:            status,
		ArticlesProcessed: in.ArticlesProcessed,
		UniqueStories:     in.TotalStories,
		VerifiedStories:   in.VerifiedStories,
		RejectedStories:   in.RejectedStories,
		Selected:          in.Selection.Selected,
		LLMRequests:       len(in.Usage),
		InputTokens:       inTok,
		OutputTokens:      outTok,
		TotalCostUSD:      cost,
		DurationMS:        in.DurationMS,
	}, nil
}

func (a *EditorialActivities) requireLLM() error {
	if a.LLM == nil {
		return temporal.NewNonRetryableApplicationError("llm provider is not configured (set LLM_API_KEY)", "LLM_CONFIG_ERROR", nil)
	}
	if a.Prompts == nil {
		return temporal.NewNonRetryableApplicationError("prompt registry is nil", "PROMPT_CONFIG_ERROR", nil)
	}
	return nil
}

func (a *EditorialActivities) recordUsageMetrics(u editor.LLMUsageRecord) {
	if a.Metrics == nil {
		return
	}
	a.Metrics.EditorLLMRequestsTotal.WithLabelValues(u.Operation, u.Model, u.Status).Inc()
	if u.Status != "OK" {
		a.Metrics.EditorLLMErrorsTotal.WithLabelValues(u.Operation, u.Status).Inc()
	}
	a.Metrics.EditorLLMTokensTotal.WithLabelValues(u.Operation, u.Model, "input").Add(float64(u.InputTokens))
	a.Metrics.EditorLLMTokensTotal.WithLabelValues(u.Operation, u.Model, "output").Add(float64(u.OutputTokens))
	a.Metrics.EditorLLMCostUSDTotal.WithLabelValues(u.Operation, u.Model).Add(u.CostUSD)
}
