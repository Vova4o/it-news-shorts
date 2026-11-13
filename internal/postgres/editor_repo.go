package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/editor"
)

// ErrEditorialNotFound is returned when a requested selection, story, or run does not exist.
var ErrEditorialNotFound = errors.New("editorial record not found")

// EditorialRunInput describes parameters for starting or resuming an editorial run.
type EditorialRunInput struct {
	ID            string
	SnapshotID    string
	RunDate       time.Time
	Status        string
	Model         string
	PromptVersion string
}

// EditorialRunRecord mirrors a row in editorial_runs.
type EditorialRunRecord struct {
	ID              string     `json:"id"`
	SnapshotID      string     `json:"snapshot_id"`
	RunDate         time.Time  `json:"run_date"`
	Status          string     `json:"status"`
	Model           string     `json:"model"`
	PromptVersion   string     `json:"prompt_version"`
	TotalArticles   int        `json:"total_articles"`
	TotalStories    int        `json:"total_stories"`
	VerifiedStories int        `json:"verified_stories"`
	RejectedStories int        `json:"rejected_stories"`
	SelectedCount   int        `json:"selected_count"`
	TotalCostUSD    float64    `json:"total_cost_usd"`
	ErrorMessage    string     `json:"error_message,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	CompletedAt     *time.Time `json:"completed_at,omitempty"`
}

// SaveEditorialResultsParams groups everything persisted at the end of an editorial run.
type SaveEditorialResultsParams struct {
	RunID         string
	SnapshotID    string
	RunDate       time.Time
	Status        string
	Model         string
	PromptVersion string
	Stories       []editor.ClusteredStory
	Selection     editor.SelectionResult
	Report        editor.EditorialReport
	Usage         []editor.LLMUsageRecord
}

// DailySelectionView is returned by GetDailySelection for CLI/API inspection.
type DailySelectionView struct {
	SelectionID string                     `json:"selection_id"`
	Date        string                     `json:"date"`
	RunID       string                     `json:"editorial_run_id"`
	SnapshotID  string                     `json:"snapshot_id"`
	Summary     string                     `json:"summary_notes"`
	Items       []editor.SelectedStoryItem `json:"items"`
}

// StoryExplanationView provides full auditability for one story.
type StoryExplanationView struct {
	StoryID            string             `json:"story_id"`
	Title              string             `json:"title"`
	Category           string             `json:"category"`
	ClusterConfidence  float64            `json:"cluster_confidence"`
	ClusterReason      string             `json:"cluster_reason"`
	VerificationStatus string             `json:"verification_status"`
	RejectionReason    string             `json:"rejection_reason,omitempty"`
	Score              *editor.StoryScore `json:"score,omitempty"`
	ArticleIDs         []string           `json:"article_ids"`
	Claims             []editor.Claim     `json:"claims"`
}

// EditorialStatsView summarizes editorial metrics for a date.
type EditorialStatsView struct {
	Date            string  `json:"date"`
	Runs            int     `json:"runs"`
	TotalArticles   int     `json:"total_articles"`
	TotalStories    int     `json:"total_stories"`
	VerifiedStories int     `json:"verified_stories"`
	RejectedStories int     `json:"rejected_stories"`
	SelectedCount   int     `json:"selected_count"`
	LLMRequests     int     `json:"llm_requests"`
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
}

// EditorRepository persists and queries AI editorial runs, stories, claims,
// evidence, scores, TOP-3 selections, and LLM usage telemetry.
type EditorRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewEditorRepository creates an EditorRepository.
func NewEditorRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *EditorRepository {
	if queryTimeout <= 0 {
		queryTimeout = 10 * time.Second
	}
	return &EditorRepository{pool: pool, timeout: queryTimeout}
}

// CreateOrGetEditorialRun idempotently creates or fetches an editorial_runs row.
func (r *EditorRepository) CreateOrGetEditorialRun(ctx context.Context, in EditorialRunInput) (EditorialRunRecord, error) {
	if in.ID == "" {
		return EditorialRunRecord{}, domain.MissingField("editorial_run.id")
	}
	if in.SnapshotID == "" {
		return EditorialRunRecord{}, domain.MissingField("editorial_run.snapshot_id")
	}
	if in.Status == "" {
		in.Status = "RUNNING"
	}
	if in.PromptVersion == "" {
		in.PromptVersion = "v1"
	}

	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var rec EditorialRunRecord
	err := r.pool.QueryRow(cctx, `
		INSERT INTO editorial_runs (id, snapshot_id, run_date, status, model, prompt_version)
		VALUES ($1, $2, $3::date, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET updated_at = NOW()
		RETURNING id, snapshot_id, run_date, status, model, prompt_version,
		          total_articles, total_stories, verified_stories, rejected_stories,
		          selected_count, total_cost_usd, error_message, started_at, completed_at
	`, in.ID, in.SnapshotID, in.RunDate.UTC().Format("2006-01-02"), in.Status, in.Model, in.PromptVersion).Scan(
		&rec.ID, &rec.SnapshotID, &rec.RunDate, &rec.Status, &rec.Model, &rec.PromptVersion,
		&rec.TotalArticles, &rec.TotalStories, &rec.VerifiedStories, &rec.RejectedStories,
		&rec.SelectedCount, &rec.TotalCostUSD, &rec.ErrorMessage, &rec.StartedAt, &rec.CompletedAt,
	)
	if err != nil {
		return EditorialRunRecord{}, fmt.Errorf("create or get editorial run %s: %w", in.ID, err)
	}
	return rec, nil
}

// SaveEditorialResults atomically saves stories, article links, claims, evidence,
// scores, daily TOP-3 selection, LLM usage, and the completed editorial run.
func (r *EditorRepository) SaveEditorialResults(ctx context.Context, p SaveEditorialResultsParams) error {
	if p.RunID == "" {
		return domain.MissingField("editorial_run.id")
	}
	if p.SnapshotID == "" {
		return domain.MissingField("editorial_run.snapshot_id")
	}

	cctx, cancel := context.WithTimeout(ctx, r.timeout*3)
	defer cancel()

	tx, err := r.pool.BeginTx(cctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin editorial transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(cctx) }()

	reportBytes, err := json.Marshal(p.Report)
	if err != nil {
		return fmt.Errorf("marshal editorial report: %w", err)
	}

	dateStr := p.RunDate.UTC().Format("2006-01-02")
	status := p.Status
	if status == "" {
		status = "COMPLETED"
	}

	_, err = tx.Exec(cctx, `
		INSERT INTO editorial_runs (
			id, snapshot_id, run_date, status, model, prompt_version,
			total_articles, total_stories, verified_stories, rejected_stories,
			selected_count, total_cost_usd, report_json, completed_at
		)
		VALUES ($1, $2, $3::date, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, NOW())
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			total_articles = EXCLUDED.total_articles,
			total_stories = EXCLUDED.total_stories,
			verified_stories = EXCLUDED.verified_stories,
			rejected_stories = EXCLUDED.rejected_stories,
			selected_count = EXCLUDED.selected_count,
			total_cost_usd = EXCLUDED.total_cost_usd,
			report_json = EXCLUDED.report_json,
			completed_at = NOW(),
			updated_at = NOW()
	`, p.RunID, p.SnapshotID, dateStr, status, p.Model, p.PromptVersion,
		p.Report.ArticlesProcessed, p.Report.UniqueStories, p.Report.VerifiedStories,
		p.Report.RejectedStories, len(p.Selection.Selected), p.Report.TotalCostUSD, reportBytes)
	if err != nil {
		return fmt.Errorf("upsert editorial run %s: %w", p.RunID, err)
	}

	selectedRank := make(map[string]int, len(p.Selection.Selected))
	for _, item := range p.Selection.Selected {
		selectedRank[item.StoryID] = item.Rank
	}

	for _, st := range p.Stories {
		rank := selectedRank[string(st.ID)]
		storyStatus := "CANDIDATE"
		if !st.VerificationPassed {
			storyStatus = "REJECTED"
		} else if rank > 0 {
			storyStatus = "SELECTED"
		}
		verifStatus := string(editor.StatusUnverified)
		if st.VerificationPassed {
			verifStatus = string(editor.StatusVerified)
		}
		normScore := 0.0
		if st.Score != nil {
			normScore = st.Score.FinalScore / 100.0
			if normScore < 0 {
				normScore = 0
			} else if normScore > 1 {
				normScore = 1
			}
		}

		_, err = tx.Exec(cctx, `
			INSERT INTO stories (
				id, editorial_run_id, snapshot_id, title, summary, score, position, status,
				category, cluster_confidence, cluster_reason, verification_status,
				rejection_reason, story_fingerprint, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, NOW())
			ON CONFLICT (id) DO UPDATE SET
				editorial_run_id = EXCLUDED.editorial_run_id,
				snapshot_id = EXCLUDED.snapshot_id,
				title = EXCLUDED.title,
				summary = EXCLUDED.summary,
				score = EXCLUDED.score,
				position = EXCLUDED.position,
				status = EXCLUDED.status,
				category = EXCLUDED.category,
				cluster_confidence = EXCLUDED.cluster_confidence,
				cluster_reason = EXCLUDED.cluster_reason,
				verification_status = EXCLUDED.verification_status,
				rejection_reason = EXCLUDED.rejection_reason,
				story_fingerprint = EXCLUDED.story_fingerprint,
				updated_at = NOW()
		`, string(st.ID), p.RunID, p.SnapshotID, st.Title, st.Summary, normScore, rank, storyStatus,
			string(st.Category), st.Confidence, st.ClusterReason, verifStatus, st.RejectionReason, st.Fingerprint)
		if err != nil {
			return fmt.Errorf("upsert story %s: %w", st.ID, err)
		}

		for _, link := range st.ArticleLinks {
			role := link.Role
			if role == "" {
				role = "CORROBORATING"
			}
			_, err = tx.Exec(cctx, `
				INSERT INTO story_articles (story_id, article_id, role, confidence, reason)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (story_id, article_id) DO UPDATE SET
					role = EXCLUDED.role,
					confidence = EXCLUDED.confidence,
					reason = EXCLUDED.reason
			`, string(st.ID), link.ArticleID, role, link.Confidence, link.Reason)
			if err != nil {
				return fmt.Errorf("upsert story_article (%s,%s): %w", st.ID, link.ArticleID, err)
			}
		}

		for _, cl := range st.Claims {
			entBytes, _ := json.Marshal(cl.Entities)
			numBytes, _ := json.Marshal(cl.Numbers)
			clStatus := string(cl.VerificationStatus)
			if clStatus == "" {
				clStatus = string(editor.StatusUnverified)
			}
			_, err = tx.Exec(cctx, `
				INSERT INTO story_claims (
					id, story_id, claim_text, claim_type, entities_json, numbers_json,
					verification_status, verification_reason, contradiction_note, confidence
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT (id) DO UPDATE SET
					claim_text = EXCLUDED.claim_text,
					verification_status = EXCLUDED.verification_status,
					verification_reason = EXCLUDED.verification_reason,
					contradiction_note = EXCLUDED.contradiction_note,
					confidence = EXCLUDED.confidence
			`, cl.ID, string(st.ID), cl.Text, string(cl.Type), entBytes, numBytes,
				clStatus, cl.RejectionReason, "", cl.Confidence)
			if err != nil {
				return fmt.Errorf("upsert story_claim %s: %w", cl.ID, err)
			}

			for _, ev := range cl.Evidence {
				evID := ev.ID
				if evID == "" {
					evID = stableHashID("ev", cl.ID+"|"+ev.ArticleID+"|"+ev.SourceURL)
				}
				var pubPtr *time.Time
				if !ev.AccessedAt.IsZero() {
					t := ev.AccessedAt
					pubPtr = &t
				}
				_, err = tx.Exec(cctx, `
					INSERT INTO claim_evidence (
						id, claim_id, article_id, source_url, source_type, quote,
						published_at, is_primary_source, is_independent
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					ON CONFLICT (id) DO UPDATE SET
						quote = EXCLUDED.quote,
						is_primary_source = EXCLUDED.is_primary_source,
						is_independent = EXCLUDED.is_independent
				`, evID, cl.ID, ev.ArticleID, ev.SourceURL, ev.SourceName, ev.Snippet, pubPtr, ev.IsPrimary, ev.IsIndependent)
				if err != nil {
					return fmt.Errorf("upsert claim_evidence %s: %w", evID, err)
				}
			}
		}

		if st.Score != nil {
			_, err = tx.Exec(cctx, `
				INSERT INTO story_scores (
					story_id, editorial_run_id, impact_score, interest_score,
					freshness_score, novelty_score, final_score, reasoning_summary,
					model, prompt_version, scored_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW())
				ON CONFLICT (story_id) DO UPDATE SET
					editorial_run_id = EXCLUDED.editorial_run_id,
					impact_score = EXCLUDED.impact_score,
					interest_score = EXCLUDED.interest_score,
					freshness_score = EXCLUDED.freshness_score,
					novelty_score = EXCLUDED.novelty_score,
					final_score = EXCLUDED.final_score,
					reasoning_summary = EXCLUDED.reasoning_summary,
					model = EXCLUDED.model,
					prompt_version = EXCLUDED.prompt_version,
					scored_at = NOW()
			`, string(st.ID), p.RunID, st.Score.Impact, st.Score.Interest,
				st.Score.Freshness, st.Score.Novelty, st.Score.FinalScore,
				st.Score.ReasoningSummary, st.Score.ModelVersion, st.Score.PromptVersion)
			if err != nil {
				return fmt.Errorf("upsert story_score %s: %w", st.ID, err)
			}
		}
	}

	selID := "sel-" + dateStr
	summaryNotes := fmt.Sprintf("Selected %d of %d stories (%d rejected)", len(p.Selection.Selected), len(p.Stories), len(p.Selection.Rejected))
	_, err = tx.Exec(cctx, `
		INSERT INTO daily_selections (id, selection_date, editorial_run_id, snapshot_id, selected_count, summary_notes)
		VALUES ($1, $2::date, $3, $4, $5, $6)
		ON CONFLICT (selection_date) DO UPDATE SET
			editorial_run_id = EXCLUDED.editorial_run_id,
			snapshot_id = EXCLUDED.snapshot_id,
			selected_count = EXCLUDED.selected_count,
			summary_notes = EXCLUDED.summary_notes,
			updated_at = NOW()
	`, selID, dateStr, p.RunID, p.SnapshotID, len(p.Selection.Selected), summaryNotes)
	if err != nil {
		return fmt.Errorf("upsert daily_selection %s: %w", selID, err)
	}

	if _, err = tx.Exec(cctx, `DELETE FROM daily_selection_items WHERE selection_id = $1`, selID); err != nil {
		return fmt.Errorf("clear daily_selection_items %s: %w", selID, err)
	}
	for _, item := range p.Selection.Selected {
		_, err = tx.Exec(cctx, `
			INSERT INTO daily_selection_items (
				selection_id, rank, story_id, story_fingerprint, category, final_score, selection_reason
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
		`, selID, item.Rank, item.StoryID, item.Fingerprint, item.Category, item.Score, item.SelectionReason)
		if err != nil {
			return fmt.Errorf("insert daily_selection_item rank %d: %w", item.Rank, err)
		}
	}

	for i, u := range p.Usage {
		uid := stableHashID("llm", fmt.Sprintf("%s|%s|%d|%s", p.RunID, u.Operation, i, u.RequestID))
		_, err = tx.Exec(cctx, `
			INSERT INTO llm_usage (
				id, editorial_run_id, operation, provider, model, prompt_version,
				request_id, input_tokens, output_tokens, cost_usd, duration_ms, status, error_message
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (id) DO NOTHING
		`, uid, p.RunID, u.Operation, u.Provider, u.Model, u.PromptVersion,
			u.RequestID, u.InputTokens, u.OutputTokens, u.CostUSD, u.DurationMS, u.Status, u.ErrorMessage)
		if err != nil {
			return fmt.Errorf("insert llm_usage %s: %w", uid, err)
		}
	}

	if err := tx.Commit(cctx); err != nil {
		return fmt.Errorf("commit editorial results: %w", err)
	}
	return nil
}

// GetPreviouslyPublishedFingerprints returns story fingerprints selected in the
// last lookbackDays before beforeDate so repeat stories are avoided.
func (r *EditorRepository) GetPreviouslyPublishedFingerprints(ctx context.Context, beforeDate time.Time, lookbackDays int) (map[string]bool, error) {
	if lookbackDays <= 0 {
		lookbackDays = 7
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(cctx, `
		SELECT DISTINCT dsi.story_fingerprint
		FROM daily_selection_items dsi
		JOIN daily_selections ds ON ds.id = dsi.selection_id
		WHERE ds.selection_date < $1::date
		  AND ds.selection_date >= ($1::date - ($2::int * INTERVAL '1 day'))
	`, beforeDate.UTC().Format("2006-01-02"), lookbackDays)
	if err != nil {
		return nil, fmt.Errorf("query previously published fingerprints: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, fmt.Errorf("scan fingerprint: %w", err)
		}
		if fp != "" {
			out[fp] = true
		}
	}
	return out, rows.Err()
}

// GetDailySelection returns the persisted TOP-3 selection for a date.
func (r *EditorRepository) GetDailySelection(ctx context.Context, date time.Time) (*DailySelectionView, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	dateStr := date.UTC().Format("2006-01-02")
	var view DailySelectionView
	err := r.pool.QueryRow(cctx, `
		SELECT id, to_char(selection_date, 'YYYY-MM-DD'), editorial_run_id, snapshot_id, summary_notes
		FROM daily_selections
		WHERE selection_date = $1::date
	`, dateStr).Scan(&view.SelectionID, &view.Date, &view.RunID, &view.SnapshotID, &view.Summary)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEditorialNotFound
		}
		return nil, fmt.Errorf("query daily selection %s: %w", dateStr, err)
	}

	rows, err := r.pool.Query(cctx, `
		SELECT dsi.rank, dsi.story_id, s.title, dsi.category, dsi.final_score, dsi.selection_reason, dsi.story_fingerprint
		FROM daily_selection_items dsi
		JOIN stories s ON s.id = dsi.story_id
		WHERE dsi.selection_id = $1
		ORDER BY dsi.rank ASC
	`, view.SelectionID)
	if err != nil {
		return nil, fmt.Errorf("query daily selection items %s: %w", view.SelectionID, err)
	}
	defer rows.Close()

	for rows.Next() {
		var item editor.SelectedStoryItem
		if err := rows.Scan(&item.Rank, &item.StoryID, &item.Title, &item.Category, &item.Score, &item.SelectionReason, &item.Fingerprint); err != nil {
			return nil, fmt.Errorf("scan selection item: %w", err)
		}
		view.Items = append(view.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate selection items: %w", err)
	}
	return &view, nil
}

// ExplainStory returns the full clustering, verification, claims, evidence, and score for a story.
func (r *EditorRepository) ExplainStory(ctx context.Context, storyID string) (*StoryExplanationView, error) {
	if storyID == "" {
		return nil, domain.MissingField("story_id")
	}
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var view StoryExplanationView
	err := r.pool.QueryRow(cctx, `
		SELECT id, title, category, cluster_confidence, cluster_reason, verification_status, rejection_reason
		FROM stories
		WHERE id = $1
	`, storyID).Scan(&view.StoryID, &view.Title, &view.Category, &view.ClusterConfidence,
		&view.ClusterReason, &view.VerificationStatus, &view.RejectionReason)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEditorialNotFound
		}
		return nil, fmt.Errorf("query story %s: %w", storyID, err)
	}

	artRows, err := r.pool.Query(cctx, `SELECT article_id FROM story_articles WHERE story_id = $1 ORDER BY article_id`, storyID)
	if err != nil {
		return nil, fmt.Errorf("query story articles %s: %w", storyID, err)
	}
	for artRows.Next() {
		var aid string
		if err := artRows.Scan(&aid); err != nil {
			artRows.Close()
			return nil, fmt.Errorf("scan story article: %w", err)
		}
		view.ArticleIDs = append(view.ArticleIDs, aid)
	}
	artRows.Close()

	var sc editor.StoryScore
	err = r.pool.QueryRow(cctx, `
		SELECT story_id, impact_score, interest_score, freshness_score, novelty_score,
		       final_score, reasoning_summary, model, prompt_version, scored_at
		FROM story_scores WHERE story_id = $1
	`, storyID).Scan(&sc.StoryID, &sc.Impact, &sc.Interest, &sc.Freshness, &sc.Novelty,
		&sc.FinalScore, &sc.ReasoningSummary, &sc.ModelVersion, &sc.PromptVersion, &sc.CalculatedAt)
	if err == nil {
		view.Score = &sc
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("query story score %s: %w", storyID, err)
	}

	clRows, err := r.pool.Query(cctx, `
		SELECT id, story_id, claim_text, claim_type, verification_status, verification_reason, confidence
		FROM story_claims WHERE story_id = $1 ORDER BY id
	`, storyID)
	if err != nil {
		return nil, fmt.Errorf("query story claims %s: %w", storyID, err)
	}
	defer clRows.Close()
	for clRows.Next() {
		var cl editor.Claim
		var clType, clStatus string
		if err := clRows.Scan(&cl.ID, &cl.StoryID, &cl.Text, &clType, &clStatus, &cl.RejectionReason, &cl.Confidence); err != nil {
			return nil, fmt.Errorf("scan claim: %w", err)
		}
		cl.Type = editor.ClaimType(clType)
		cl.VerificationStatus = editor.VerificationStatus(clStatus)
		view.Claims = append(view.Claims, cl)
	}
	return &view, clRows.Err()
}

// GetEditorialStats aggregates editorial run and LLM cost telemetry for a date.
func (r *EditorRepository) GetEditorialStats(ctx context.Context, date time.Time) (*EditorialStatsView, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	dateStr := date.UTC().Format("2006-01-02")
	stats := &EditorialStatsView{Date: dateStr}
	err := r.pool.QueryRow(cctx, `
		SELECT COUNT(*), COALESCE(SUM(total_articles), 0), COALESCE(SUM(total_stories), 0),
		       COALESCE(SUM(verified_stories), 0), COALESCE(SUM(rejected_stories), 0),
		       COALESCE(SUM(selected_count), 0)
		FROM editorial_runs
		WHERE run_date = $1::date
	`, dateStr).Scan(&stats.Runs, &stats.TotalArticles, &stats.TotalStories,
		&stats.VerifiedStories, &stats.RejectedStories, &stats.SelectedCount)
	if err != nil {
		return nil, fmt.Errorf("query editorial run stats %s: %w", dateStr, err)
	}

	err = r.pool.QueryRow(cctx, `
		SELECT COUNT(*), COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
		       COALESCE(SUM(u.cost_usd), 0)
		FROM llm_usage u
		JOIN editorial_runs er ON er.id = u.editorial_run_id
		WHERE er.run_date = $1::date
	`, dateStr).Scan(&stats.LLMRequests, &stats.InputTokens, &stats.OutputTokens, &stats.TotalCostUSD)
	if err != nil {
		return nil, fmt.Errorf("query llm usage stats %s: %w", dateStr, err)
	}
	return stats, nil
}

// GetTodayLLMSpendUSD returns total LLM spend in USD for the UTC day of now.
func (r *EditorRepository) GetTodayLLMSpendUSD(ctx context.Context, now time.Time) (float64, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	var spend float64
	err := r.pool.QueryRow(cctx, `
		SELECT COALESCE(SUM(cost_usd), 0)
		FROM llm_usage
		WHERE created_at >= date_trunc('day', $1::timestamptz AT TIME ZONE 'UTC')
	`, now.UTC()).Scan(&spend)
	if err != nil {
		return 0, fmt.Errorf("query today llm spend: %w", err)
	}
	return spend, nil
}

// RecordLLMUsage inserts a single LLM usage telemetry row.
func (r *EditorRepository) RecordLLMUsage(ctx context.Context, u editor.LLMUsageRecord) error {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	uid := stableHashID("llm", fmt.Sprintf("%s|%s|%s|%d", u.EditorialRunID, u.Operation, u.RequestID, time.Now().UnixNano()))
	var runPtr *string
	if u.EditorialRunID != "" {
		runPtr = &u.EditorialRunID
	}
	_, err := r.pool.Exec(cctx, `
		INSERT INTO llm_usage (
			id, editorial_run_id, operation, provider, model, prompt_version,
			request_id, input_tokens, output_tokens, cost_usd, duration_ms, status, error_message
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, uid, runPtr, u.Operation, u.Provider, u.Model, u.PromptVersion,
		u.RequestID, u.InputTokens, u.OutputTokens, u.CostUSD, u.DurationMS, u.Status, u.ErrorMessage)
	if err != nil {
		return fmt.Errorf("insert llm usage: %w", err)
	}
	return nil
}

func stableHashID(prefix, raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return prefix + "-" + hex.EncodeToString(sum[:12])
}
