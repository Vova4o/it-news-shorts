package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// StoryRepository provides minimal story persistence needed for foreign-key
// integrity in smoke and workflow tests.
type StoryRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewStoryRepository creates a StoryRepository.
func NewStoryRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *StoryRepository {
	return &StoryRepository{pool: pool, timeout: queryTimeout}
}

// EnsureSmokeStory idempotently creates a Story row tied to an existing DailyRun
// so smoke VideoJob and MediaAsset records satisfy all foreign keys.
func (r *StoryRepository) EnsureSmokeStory(ctx context.Context, story domain.Story) error {
	if story.ID == "" {
		return domain.MissingField("story.id")
	}
	if story.DailyRunID == "" {
		return domain.MissingField("story.daily_run_id")
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	_, err := r.pool.Exec(qCtx, `
		INSERT INTO stories (id, daily_run_id, title, summary, score, position, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'SMOKE', $7, $7)
		ON CONFLICT (id) DO NOTHING
	`,
		story.ID.String(),
		story.DailyRunID.String(),
		story.Title,
		story.Summary,
		story.Score,
		story.Position,
		story.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("postgres: ensure smoke story: %w", mapPGError("insert story", err))
	}
	return nil
}
