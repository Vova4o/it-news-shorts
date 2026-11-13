package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
	"github.com/it-news-shorts/it-news-shorts/internal/news"
)

// SourceFetchState tracks conditional HTTP caching and health per source.
type SourceFetchState struct {
	SourceID            string
	ETag                string
	LastModified        string
	LastFetchAt         *time.Time
	LastSuccessAt       *time.Time
	LastError           string
	ConsecutiveFailures int
	UpdatedAt           time.Time
}

// SourceWithState combines a configured source with its latest fetch state.
type SourceWithState struct {
	Config news.SourceConfig
	State  SourceFetchState
}

// SourceFetchHistory records one ingestion attempt for a source.
type SourceFetchHistory struct {
	SourceID       string
	WorkflowID     string
	Status         string // SUCCESS, NOT_MODIFIED, ERROR
	FetchedCount   int
	InsertedCount  int
	DuplicateCount int
	ErrorMessage   string
	DurationMS     int64
}

// DailySnapshot represents a persisted 24h news snapshot.
type DailySnapshot struct {
	ID             string
	SnapshotDate   time.Time
	Window         domain.TimeWindow
	CoverageStatus string // FULL_COVERAGE or PARTIAL_COVERAGE
	ArticleCount   int
	SourceStats    map[string]int
	WorkflowID     string
	CreatedAt      time.Time
	ArticleIDs     []string
}

// IngestionStats summarizes recent news collection metrics from PostgreSQL.
type IngestionStats struct {
	TotalSources      int            `json:"total_sources"`
	EnabledSources    int            `json:"enabled_sources"`
	FetchedCount      int            `json:"fetched_count"`
	InsertedCount     int            `json:"inserted_count"`
	DuplicateCount    int            `json:"duplicate_count"`
	ErrorCount        int            `json:"error_count"`
	ArticlesLast24h   int            `json:"articles_last_24h"`
	TotalDurationMS   int64          `json:"total_duration_ms"`
	CoverageStatus    string         `json:"coverage_status"`
	EarliestFetchTime *time.Time     `json:"earliest_fetch_time,omitempty"`
	ArticlesBySource  map[string]int `json:"articles_by_source"`
}

// NewsRepository persists news sources, normalized articles, deduplication
// references, fetch state, and daily snapshots in PostgreSQL.
type NewsRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewNewsRepository creates a NewsRepository.
func NewNewsRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *NewsRepository {
	return &NewsRepository{pool: pool, timeout: queryTimeout}
}

// EnsureSources upserts configured sources without overwriting operator-toggled
// enabled flags on existing rows.
func (r *NewsRepository) EnsureSources(ctx context.Context, sources []news.SourceConfig) error {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: begin ensure sources tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	for _, s := range sources {
		if err := s.Validate(); err != nil {
			return err
		}
		if _, err := tx.Exec(qCtx, `
			INSERT INTO sources (id, name, source_type, base_url, enabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name,
				source_type = EXCLUDED.source_type,
				base_url = EXCLUDED.base_url,
				updated_at = NOW()
		`, s.ID, s.Name, string(s.Type), s.FeedURL, s.Enabled); err != nil {
			return mapPGError("postgres: ensure source", err)
		}
		if _, err := tx.Exec(qCtx, `
			INSERT INTO source_fetch_state (source_id, updated_at)
			VALUES ($1, NOW())
			ON CONFLICT (source_id) DO NOTHING
		`, s.ID); err != nil {
			return mapPGError("postgres: ensure source fetch state", err)
		}
	}

	if err := tx.Commit(qCtx); err != nil {
		return fmt.Errorf("postgres: commit ensure sources tx: %w", err)
	}
	return nil
}

// ListSources returns all sources joined with their fetch state.
func (r *NewsRepository) ListSources(ctx context.Context) ([]SourceWithState, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(qCtx, `
		SELECT s.id, s.name, s.source_type, s.base_url, s.enabled,
		       COALESCE(st.etag, ''), COALESCE(st.last_modified, ''),
		       st.last_fetch_at, st.last_success_at,
		       COALESCE(st.last_error, ''), COALESCE(st.consecutive_failures, 0),
		       COALESCE(st.updated_at, s.updated_at)
		FROM sources s
		LEFT JOIN source_fetch_state st ON st.source_id = s.id
		ORDER BY s.id ASC
	`)
	if err != nil {
		return nil, mapPGError("postgres: list sources", err)
	}
	defer rows.Close()

	var out []SourceWithState
	for rows.Next() {
		var (
			item        SourceWithState
			sourceType  string
			lastFetch   *time.Time
			lastSuccess *time.Time
		)
		if err := rows.Scan(
			&item.Config.ID,
			&item.Config.Name,
			&sourceType,
			&item.Config.FeedURL,
			&item.Config.Enabled,
			&item.State.ETag,
			&item.State.LastModified,
			&lastFetch,
			&lastSuccess,
			&item.State.LastError,
			&item.State.ConsecutiveFailures,
			&item.State.UpdatedAt,
		); err != nil {
			return nil, mapPGError("postgres: scan source", err)
		}
		item.Config.Type = news.SourceType(sourceType)
		item.Config.Language = domain.LanguageEnglish
		item.State.SourceID = item.Config.ID
		if lastFetch != nil {
			t := lastFetch.UTC()
			item.State.LastFetchAt = &t
		}
		if lastSuccess != nil {
			t := lastSuccess.UTC()
			item.State.LastSuccessAt = &t
		}
		item.State.UpdatedAt = item.State.UpdatedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate sources", err)
	}
	return out, nil
}

// UpsertArticle atomically deduplicates by (source_id, external_id) and
// canonical_url, updates engagement metrics, and records source provenance.
func (r *NewsRepository) UpsertArticle(ctx context.Context, a news.UnifiedArticle) (string, bool, error) {
	norm, err := news.NormalizeArticle(a)
	if err != nil {
		return "", false, err
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return "", false, fmt.Errorf("postgres: begin upsert article tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	existingID, found, err := findExistingArticleTx(qCtx, tx, norm.SourceID, norm.ExternalID, norm.CanonicalURL)
	if err != nil {
		return "", false, err
	}

	inserted := false
	articleID := existingID
	if !found {
		articleID = norm.ID.String()
		catsJSON, err := json.Marshal(norm.Categories)
		if err != nil {
			return "", false, fmt.Errorf("postgres: marshal categories: %w", err)
		}
		metaJSON, err := json.Marshal(norm.RawMetadata)
		if err != nil {
			return "", false, fmt.Errorf("postgres: marshal raw_metadata: %w", err)
		}

		tag, err := tx.Exec(qCtx, `
			INSERT INTO articles (
				id, source_id, external_id, title, url, summary, content, language,
				published_at, fetched_at, created_at,
				original_url, canonical_url, normalized_title, source_type, author,
				categories, popularity_score, comments_count, source_submitted_at,
				discovered_at, raw_metadata, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, '', $7,
				$8, $9, $9,
				$10, $11, $12, $13, $14,
				$15, $16, $17, $18,
				$19, $20, $9
			)
			ON CONFLICT DO NOTHING
		`,
			articleID,
			norm.SourceID,
			norm.ExternalID,
			norm.Title,
			norm.CanonicalURL,
			norm.Description,
			string(norm.Language),
			nullTimePtr(norm.PublishedAt),
			norm.FetchedAt.UTC(),
			norm.OriginalURL,
			norm.CanonicalURL,
			norm.NormalizedTitle,
			string(norm.SourceType),
			norm.Author,
			catsJSON,
			norm.PopularityScore,
			norm.CommentsCount,
			nullTimePtr(norm.SourceSubmittedAt),
			norm.DiscoveredAt.UTC(),
			metaJSON,
		)
		if err != nil {
			return "", false, mapPGError("postgres: insert article", err)
		}
		if tag.RowsAffected() == 1 {
			inserted = true
		} else {
			// Concurrent insert won the race; look up the winning row.
			raceID, raceFound, err := findExistingArticleTx(qCtx, tx, norm.SourceID, norm.ExternalID, norm.CanonicalURL)
			if err != nil {
				return "", false, err
			}
			if !raceFound {
				return "", false, fmt.Errorf("postgres: article conflict without matching row for %s", norm.CanonicalURL)
			}
			articleID = raceID
		}
	}

	if !inserted {
		if _, err := tx.Exec(qCtx, `
			UPDATE articles SET
				published_at = COALESCE(articles.published_at, $2),
				source_submitted_at = COALESCE(articles.source_submitted_at, $3),
				popularity_score = CASE
					WHEN $4::INTEGER IS NULL THEN articles.popularity_score
					WHEN articles.popularity_score IS NULL THEN $4::INTEGER
					ELSE GREATEST(articles.popularity_score, $4::INTEGER)
				END,
				comments_count = CASE
					WHEN $5::INTEGER IS NULL THEN articles.comments_count
					WHEN articles.comments_count IS NULL THEN $5::INTEGER
					ELSE GREATEST(articles.comments_count, $5::INTEGER)
				END,
				summary = CASE WHEN articles.summary = '' THEN $6 ELSE articles.summary END,
				author = CASE WHEN articles.author = '' THEN $7 ELSE articles.author END,
				fetched_at = GREATEST(articles.fetched_at, $8),
				updated_at = NOW()
			WHERE id = $1
		`,
			articleID,
			nullTimePtr(norm.PublishedAt),
			nullTimePtr(norm.SourceSubmittedAt),
			norm.PopularityScore,
			norm.CommentsCount,
			norm.Description,
			norm.Author,
			norm.FetchedAt.UTC(),
		); err != nil {
			return "", false, mapPGError("postgres: update duplicate article metrics", err)
		}
	}

	if _, err := tx.Exec(qCtx, `
		INSERT INTO article_sources (
			article_id, source_id, external_id, original_url,
			popularity_score, comments_count, source_submitted_at, discovered_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (article_id, source_id) DO UPDATE SET
			external_id = CASE WHEN article_sources.external_id = '' THEN EXCLUDED.external_id ELSE article_sources.external_id END,
			popularity_score = COALESCE(EXCLUDED.popularity_score, article_sources.popularity_score),
			comments_count = COALESCE(EXCLUDED.comments_count, article_sources.comments_count),
			source_submitted_at = COALESCE(article_sources.source_submitted_at, EXCLUDED.source_submitted_at)
	`,
		articleID,
		norm.SourceID,
		norm.ExternalID,
		norm.OriginalURL,
		norm.PopularityScore,
		norm.CommentsCount,
		nullTimePtr(norm.SourceSubmittedAt),
		norm.DiscoveredAt.UTC(),
	); err != nil {
		return "", false, mapPGError("postgres: upsert article_source", err)
	}

	if err := tx.Commit(qCtx); err != nil {
		return "", false, fmt.Errorf("postgres: commit upsert article tx: %w", err)
	}
	return articleID, inserted, nil
}

func findExistingArticleTx(ctx context.Context, tx pgx.Tx, sourceID, externalID, canonicalURL string) (string, bool, error) {
	if sourceID != "" && externalID != "" {
		var id string
		err := tx.QueryRow(ctx, `
			SELECT id FROM articles WHERE source_id = $1 AND external_id = $2 FOR UPDATE
		`, sourceID, externalID).Scan(&id)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, mapPGError("postgres: find article by external_id", err)
		}
	}
	if canonicalURL != "" {
		var id string
		err := tx.QueryRow(ctx, `
			SELECT id FROM articles WHERE canonical_url = $1 OR url = $1 LIMIT 1 FOR UPDATE
		`, canonicalURL).Scan(&id)
		if err == nil {
			return id, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, mapPGError("postgres: find article by canonical_url", err)
		}
	}
	return "", false, nil
}

// GetArticleByExternalID fetches an article by (source_id, external_id).
func (r *NewsRepository) GetArticleByExternalID(ctx context.Context, sourceID, externalID string) (news.UnifiedArticle, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	row := r.pool.QueryRow(qCtx, articleSelectSQL+` WHERE source_id = $1 AND external_id = $2`, sourceID, externalID)
	art, err := scanUnifiedArticle(row)
	if err != nil {
		return news.UnifiedArticle{}, mapPGError("postgres: get article by external_id", err)
	}
	return art, nil
}

// GetArticlesByTimeRange returns articles within the half-open interval [start, end).
func (r *NewsRepository) GetArticlesByTimeRange(ctx context.Context, start, end time.Time, includeSubmittedFallback bool) ([]news.UnifiedArticle, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	query := articleSelectSQL + ` WHERE published_at >= $1 AND published_at < $2 ORDER BY published_at DESC`
	if includeSubmittedFallback {
		query = articleSelectSQL + `
			WHERE COALESCE(published_at, source_submitted_at) >= $1
			  AND COALESCE(published_at, source_submitted_at) < $2
			ORDER BY COALESCE(published_at, source_submitted_at) DESC
		`
	}

	rows, err := r.pool.Query(qCtx, query, start.UTC(), end.UTC())
	if err != nil {
		return nil, mapPGError("postgres: get articles by time range", err)
	}
	defer rows.Close()

	return scanUnifiedArticles(rows)
}

// GetArticlesBySource returns recent articles for a specific source.
func (r *NewsRepository) GetArticlesBySource(ctx context.Context, sourceID string, limit int) ([]news.UnifiedArticle, error) {
	if limit <= 0 {
		limit = 50
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(qCtx, articleSelectSQL+`
		WHERE source_id = $1
		ORDER BY COALESCE(published_at, source_submitted_at, fetched_at) DESC
		LIMIT $2
	`, sourceID, limit)
	if err != nil {
		return nil, mapPGError("postgres: get articles by source", err)
	}
	defer rows.Close()

	return scanUnifiedArticles(rows)
}

// ListRecentArticles lists articles since the given timestamp, optionally
// filtered by sourceID.
func (r *NewsRepository) ListRecentArticles(ctx context.Context, since time.Time, sourceID string, limit int) ([]news.UnifiedArticle, error) {
	if limit <= 0 {
		limit = 100
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	var (
		rows pgx.Rows
		err  error
	)
	if strings.TrimSpace(sourceID) != "" {
		rows, err = r.pool.Query(qCtx, articleSelectSQL+`
			WHERE source_id = $1
			  AND COALESCE(published_at, source_submitted_at, fetched_at) >= $2
			ORDER BY COALESCE(published_at, source_submitted_at, fetched_at) DESC
			LIMIT $3
		`, sourceID, since.UTC(), limit)
	} else {
		rows, err = r.pool.Query(qCtx, articleSelectSQL+`
			WHERE COALESCE(published_at, source_submitted_at, fetched_at) >= $1
			ORDER BY COALESCE(published_at, source_submitted_at, fetched_at) DESC
			LIMIT $2
		`, since.UTC(), limit)
	}
	if err != nil {
		return nil, mapPGError("postgres: list recent articles", err)
	}
	defer rows.Close()

	return scanUnifiedArticles(rows)
}

// GetSourceFetchState returns the conditional HTTP state for a source.
func (r *NewsRepository) GetSourceFetchState(ctx context.Context, sourceID string) (SourceFetchState, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	var st SourceFetchState
	err := r.pool.QueryRow(qCtx, `
		SELECT source_id, etag, last_modified, last_fetch_at, last_success_at,
		       last_error, consecutive_failures, updated_at
		FROM source_fetch_state
		WHERE source_id = $1
	`, sourceID).Scan(
		&st.SourceID,
		&st.ETag,
		&st.LastModified,
		&st.LastFetchAt,
		&st.LastSuccessAt,
		&st.LastError,
		&st.ConsecutiveFailures,
		&st.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SourceFetchState{SourceID: sourceID}, nil
		}
		return SourceFetchState{}, mapPGError("postgres: get source fetch state", err)
	}
	return st, nil
}

// UpdateSourceFetchState persists the latest ETag, Last-Modified, and error
// status for a source.
func (r *NewsRepository) UpdateSourceFetchState(ctx context.Context, st SourceFetchState) error {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	_, err := r.pool.Exec(qCtx, `
		INSERT INTO source_fetch_state (
			source_id, etag, last_modified, last_fetch_at, last_success_at,
			last_error, consecutive_failures, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (source_id) DO UPDATE SET
			etag = EXCLUDED.etag,
			last_modified = EXCLUDED.last_modified,
			last_fetch_at = EXCLUDED.last_fetch_at,
			last_success_at = COALESCE(EXCLUDED.last_success_at, source_fetch_state.last_success_at),
			last_error = EXCLUDED.last_error,
			consecutive_failures = EXCLUDED.consecutive_failures,
			updated_at = NOW()
	`,
		st.SourceID,
		st.ETag,
		st.LastModified,
		st.LastFetchAt,
		st.LastSuccessAt,
		st.LastError,
		st.ConsecutiveFailures,
	)
	return mapPGError("postgres: update source fetch state", err)
}

// RecordSourceFetchHistory inserts an audit record for a source fetch attempt.
func (r *NewsRepository) RecordSourceFetchHistory(ctx context.Context, h SourceFetchHistory) error {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	_, err := r.pool.Exec(qCtx, `
		INSERT INTO source_fetch_history (
			source_id, workflow_id, status, fetched_count, inserted_count,
			duplicate_count, error_message, duration_ms
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		h.SourceID,
		h.WorkflowID,
		h.Status,
		h.FetchedCount,
		h.InsertedCount,
		h.DuplicateCount,
		h.ErrorMessage,
		h.DurationMS,
	)
	return mapPGError("postgres: record source fetch history", err)
}

// GetEarliestFetchTime returns the oldest recorded fetch timestamp in
// source_fetch_history, or nil if no fetches have occurred yet.
func (r *NewsRepository) GetEarliestFetchTime(ctx context.Context) (*time.Time, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	var earliest *time.Time
	err := r.pool.QueryRow(qCtx, `
		SELECT MIN(created_at) FROM source_fetch_history WHERE status IN ('SUCCESS', 'NOT_MODIFIED')
	`).Scan(&earliest)
	if err != nil {
		return nil, mapPGError("postgres: get earliest fetch time", err)
	}
	if earliest != nil {
		t := earliest.UTC()
		return &t, nil
	}
	return nil, nil
}

// CreateDailySnapshot persists a daily snapshot and its ordered article entries
// idempotently for the given snapshot_date.
func (r *NewsRepository) CreateDailySnapshot(ctx context.Context, snap DailySnapshot, articleIDs []string) (DailySnapshot, error) {
	if err := snap.Window.Validate(); err != nil {
		return DailySnapshot{}, err
	}
	if snap.ID == "" {
		snap.ID = "snap-" + snap.SnapshotDate.UTC().Format("2006-01-02")
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return DailySnapshot{}, fmt.Errorf("postgres: begin create snapshot tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	statsJSON, err := json.Marshal(snap.SourceStats)
	if err != nil {
		return DailySnapshot{}, fmt.Errorf("postgres: marshal source_stats: %w", err)
	}
	snapDate := time.Date(snap.SnapshotDate.Year(), snap.SnapshotDate.Month(), snap.SnapshotDate.Day(), 0, 0, 0, 0, time.UTC)

	var persistedID string
	err = tx.QueryRow(qCtx, `
		INSERT INTO daily_snapshots (
			id, snapshot_date, window_start, window_end, coverage_status,
			article_count, source_stats, workflow_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (snapshot_date) DO UPDATE SET
			window_start = EXCLUDED.window_start,
			window_end = EXCLUDED.window_end,
			coverage_status = EXCLUDED.coverage_status,
			article_count = EXCLUDED.article_count,
			source_stats = EXCLUDED.source_stats,
			workflow_id = EXCLUDED.workflow_id
		RETURNING id
	`,
		snap.ID,
		snapDate,
		snap.Window.Start.UTC(),
		snap.Window.End.UTC(),
		snap.CoverageStatus,
		len(articleIDs),
		statsJSON,
		snap.WorkflowID,
	).Scan(&persistedID)
	if err != nil {
		return DailySnapshot{}, mapPGError("postgres: upsert daily_snapshot", err)
	}

	if _, err := tx.Exec(qCtx, `DELETE FROM daily_snapshot_entries WHERE snapshot_id = $1`, persistedID); err != nil {
		return DailySnapshot{}, mapPGError("postgres: clear daily_snapshot_entries", err)
	}
	for i, artID := range articleIDs {
		if _, err := tx.Exec(qCtx, `
			INSERT INTO daily_snapshot_entries (snapshot_id, article_id, position)
			VALUES ($1, $2, $3)
		`, persistedID, artID, i+1); err != nil {
			return DailySnapshot{}, mapPGError("postgres: insert daily_snapshot_entry", err)
		}
	}

	if err := tx.Commit(qCtx); err != nil {
		return DailySnapshot{}, fmt.Errorf("postgres: commit daily_snapshot tx: %w", err)
	}

	snap.ID = persistedID
	snap.ArticleCount = len(articleIDs)
	snap.ArticleIDs = append([]string(nil), articleIDs...)
	return snap, nil
}

// GetDailySnapshot fetches a persisted DailySnapshot and its article IDs by date.
func (r *NewsRepository) GetDailySnapshot(ctx context.Context, snapshotDate time.Time) (DailySnapshot, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	snapDate := time.Date(snapshotDate.Year(), snapshotDate.Month(), snapshotDate.Day(), 0, 0, 0, 0, time.UTC)
	var (
		snap      DailySnapshot
		statsRaw  []byte
		winStart  time.Time
		winEnd    time.Time
		createdAt time.Time
	)
	err := r.pool.QueryRow(qCtx, `
		SELECT id, snapshot_date, window_start, window_end, coverage_status,
		       article_count, source_stats, workflow_id, created_at
		FROM daily_snapshots
		WHERE snapshot_date = $1
	`, snapDate).Scan(
		&snap.ID,
		&snap.SnapshotDate,
		&winStart,
		&winEnd,
		&snap.CoverageStatus,
		&snap.ArticleCount,
		&statsRaw,
		&snap.WorkflowID,
		&createdAt,
	)
	if err != nil {
		return DailySnapshot{}, mapPGError("postgres: get daily_snapshot", err)
	}
	snap.Window = domain.TimeWindow{Start: winStart.UTC(), End: winEnd.UTC()}
	snap.CreatedAt = createdAt.UTC()
	if len(statsRaw) > 0 {
		_ = json.Unmarshal(statsRaw, &snap.SourceStats)
	}

	rows, err := r.pool.Query(qCtx, `
		SELECT article_id FROM daily_snapshot_entries WHERE snapshot_id = $1 ORDER BY position ASC
	`, snap.ID)
	if err != nil {
		return DailySnapshot{}, mapPGError("postgres: get daily_snapshot_entries", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return DailySnapshot{}, mapPGError("postgres: scan snapshot entry", err)
		}
		snap.ArticleIDs = append(snap.ArticleIDs, id)
	}
	if err := rows.Err(); err != nil {
		return DailySnapshot{}, mapPGError("postgres: iterate snapshot entries", err)
	}
	return snap, nil
}

// GetIngestionStats aggregates collection statistics since the given timestamp.
func (r *NewsRepository) GetIngestionStats(ctx context.Context, since time.Time) (IngestionStats, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	stats := IngestionStats{
		ArticlesBySource: make(map[string]int),
		CoverageStatus:   "PARTIAL_COVERAGE",
	}

	if err := r.pool.QueryRow(qCtx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE enabled) FROM sources
	`).Scan(&stats.TotalSources, &stats.EnabledSources); err != nil {
		return IngestionStats{}, mapPGError("postgres: count sources", err)
	}

	if err := r.pool.QueryRow(qCtx, `
		SELECT COALESCE(SUM(fetched_count), 0),
		       COALESCE(SUM(inserted_count), 0),
		       COALESCE(SUM(duplicate_count), 0),
		       COUNT(*) FILTER (WHERE status = 'ERROR'),
		       COALESCE(SUM(duration_ms), 0)
		FROM source_fetch_history
		WHERE created_at >= $1
	`, since.UTC()).Scan(
		&stats.FetchedCount,
		&stats.InsertedCount,
		&stats.DuplicateCount,
		&stats.ErrorCount,
		&stats.TotalDurationMS,
	); err != nil {
		return IngestionStats{}, mapPGError("postgres: sum fetch history", err)
	}

	rows, err := r.pool.Query(qCtx, `
		SELECT source_id, COUNT(*)
		FROM articles
		WHERE COALESCE(published_at, source_submitted_at, fetched_at) >= $1
		GROUP BY source_id
	`, since.UTC())
	if err != nil {
		return IngestionStats{}, mapPGError("postgres: count articles by source", err)
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var cnt int
		if err := rows.Scan(&src, &cnt); err != nil {
			return IngestionStats{}, mapPGError("postgres: scan articles by source", err)
		}
		stats.ArticlesBySource[src] = cnt
		stats.ArticlesLast24h += cnt
	}
	if err := rows.Err(); err != nil {
		return IngestionStats{}, mapPGError("postgres: iterate articles by source", err)
	}

	earliest, err := r.GetEarliestFetchTime(ctx)
	if err != nil {
		return IngestionStats{}, err
	}
	stats.EarliestFetchTime = earliest
	if earliest != nil && !earliest.After(since.UTC()) {
		stats.CoverageStatus = "FULL_COVERAGE"
	}

	return stats, nil
}

const articleSelectSQL = `
	SELECT id, source_id, external_id, title, normalized_title, summary,
	       original_url, canonical_url, url, source_type, author,
	       published_at, source_submitted_at, discovered_at, fetched_at,
	       language, categories, popularity_score, comments_count, raw_metadata
	FROM articles
`

func scanUnifiedArticle(row pgx.Row) (news.UnifiedArticle, error) {
	var (
		id              string
		sourceID        string
		externalID      string
		title           string
		normTitle       string
		summary         string
		originalURL     string
		canonicalURL    string
		legacyURL       string
		sourceType      string
		author          string
		publishedAt     *time.Time
		submittedAt     *time.Time
		discoveredAt    time.Time
		fetchedAt       time.Time
		language        string
		catsRaw         []byte
		popularityScore *int
		commentsCount   *int
		metaRaw         []byte
	)
	if err := row.Scan(
		&id, &sourceID, &externalID, &title, &normTitle, &summary,
		&originalURL, &canonicalURL, &legacyURL, &sourceType, &author,
		&publishedAt, &submittedAt, &discoveredAt, &fetchedAt,
		&language, &catsRaw, &popularityScore, &commentsCount, &metaRaw,
	); err != nil {
		return news.UnifiedArticle{}, err
	}
	if originalURL == "" {
		originalURL = legacyURL
	}
	if canonicalURL == "" {
		canonicalURL = legacyURL
	}

	var cats []string
	if len(catsRaw) > 0 {
		_ = json.Unmarshal(catsRaw, &cats)
	}
	var meta map[string]any
	if len(metaRaw) > 0 {
		_ = json.Unmarshal(metaRaw, &meta)
	}
	if meta == nil {
		meta = map[string]any{}
	}

	art := news.UnifiedArticle{
		ID:              domain.ArticleID(id),
		SourceID:        sourceID,
		ExternalID:      externalID,
		Title:           title,
		NormalizedTitle: normTitle,
		Description:     summary,
		OriginalURL:     originalURL,
		CanonicalURL:    canonicalURL,
		SourceType:      news.SourceType(sourceType),
		Author:          author,
		DiscoveredAt:    discoveredAt.UTC(),
		FetchedAt:       fetchedAt.UTC(),
		Language:        domain.Language(language),
		Categories:      cats,
		PopularityScore: popularityScore,
		CommentsCount:   commentsCount,
		RawMetadata:     meta,
	}
	if publishedAt != nil {
		t := publishedAt.UTC()
		art.PublishedAt = &t
	}
	if submittedAt != nil {
		t := submittedAt.UTC()
		art.SourceSubmittedAt = &t
	}
	return art, nil
}

func scanUnifiedArticles(rows pgx.Rows) ([]news.UnifiedArticle, error) {
	var out []news.UnifiedArticle
	for rows.Next() {
		art, err := scanUnifiedArticle(rows)
		if err != nil {
			return nil, mapPGError("postgres: scan article", err)
		}
		out = append(out, art)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate articles", err)
	}
	return out, nil
}

func nullTimePtr(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
