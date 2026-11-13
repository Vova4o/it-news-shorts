package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// PublicationRepository persists Publication records in PostgreSQL.
type PublicationRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewPublicationRepository creates a PublicationRepository.
func NewPublicationRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *PublicationRepository {
	return &PublicationRepository{pool: pool, timeout: queryTimeout}
}

// CreateIdempotent inserts a Publication if the idempotency_key does not yet
// exist. A duplicate publication for the same video_job_id with a different
// idempotency_key fails with ErrConflict.
func (r *PublicationRepository) CreateIdempotent(ctx context.Context, pub domain.Publication, provider string) (domain.Publication, bool, error) {
	if err := pub.Validate(); err != nil {
		return domain.Publication{}, false, err
	}
	if strings.TrimSpace(provider) == "" {
		provider = "youtube"
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return domain.Publication{}, false, fmt.Errorf("postgres: begin publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	tag, err := tx.Exec(qCtx, `
		INSERT INTO publications (
			id, video_job_id, provider, external_video_id, idempotency_key,
			status, scheduled_at, published_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		pub.ID.String(),
		pub.VideoJobID.String(),
		provider,
		pub.YouTubeVideoID,
		pub.IdempotencyKey.String(),
		string(pub.Status),
		nullTime(pub.ScheduledAt),
		nullTime(pub.PublishedAt),
		pub.CreatedAt.UTC(),
	)
	if err != nil {
		return domain.Publication{}, false, mapPGError("postgres: insert publication", err)
	}

	created := tag.RowsAffected() == 1
	if created {
		if err := insertAuditEvent(qCtx, tx, "publication", pub.ID.String(), "created", map[string]any{
			"video_job_id":    pub.VideoJobID,
			"idempotency_key": pub.IdempotencyKey,
			"status":          pub.Status,
		}); err != nil {
			return domain.Publication{}, false, err
		}
	}

	stored, err := scanPublication(tx.QueryRow(qCtx, publicationSelectSQL+` WHERE idempotency_key = $1`, pub.IdempotencyKey.String()))
	if err != nil {
		return domain.Publication{}, false, mapPGError("postgres: reload publication", err)
	}
	if err := tx.Commit(qCtx); err != nil {
		return domain.Publication{}, false, fmt.Errorf("postgres: commit publication tx: %w", err)
	}
	return stored, created, nil
}

// GetByID fetches a Publication by primary key.
func (r *PublicationRepository) GetByID(ctx context.Context, id domain.PublicationID) (domain.Publication, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	pub, err := scanPublication(r.pool.QueryRow(qCtx, publicationSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return domain.Publication{}, mapPGError("postgres: get publication by id", err)
	}
	return pub, nil
}

// GetByVideoJobID fetches the single Publication for a VideoJob.
func (r *PublicationRepository) GetByVideoJobID(ctx context.Context, jobID domain.VideoJobID) (domain.Publication, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	pub, err := scanPublication(r.pool.QueryRow(qCtx, publicationSelectSQL+` WHERE video_job_id = $1`, jobID.String()))
	if err != nil {
		return domain.Publication{}, mapPGError("postgres: get publication by video_job_id", err)
	}
	return pub, nil
}

// GetByIdempotencyKey fetches a Publication by its idempotency key.
func (r *PublicationRepository) GetByIdempotencyKey(ctx context.Context, key domain.IdempotencyKey) (domain.Publication, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	pub, err := scanPublication(r.pool.QueryRow(qCtx, publicationSelectSQL+` WHERE idempotency_key = $1`, key.String()))
	if err != nil {
		return domain.Publication{}, mapPGError("postgres: get publication by key", err)
	}
	return pub, nil
}

// UpdateStatus updates a Publication's status and timestamps.
func (r *PublicationRepository) UpdateStatus(
	ctx context.Context,
	id domain.PublicationID,
	status domain.PublicationStatus,
	externalVideoID string,
	scheduledAt, publishedAt, updatedAt time.Time,
) (domain.Publication, error) {
	if !status.Valid() {
		return domain.Publication{}, domain.InvalidValue("publication.status")
	}
	if updatedAt.IsZero() {
		return domain.Publication{}, domain.MissingField("publication.updated_at")
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return domain.Publication{}, fmt.Errorf("postgres: begin update publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	tag, err := tx.Exec(qCtx, `
		UPDATE publications
		SET status = $1,
			external_video_id = COALESCE(NULLIF($2, ''), external_video_id),
			scheduled_at = COALESCE($3, scheduled_at),
			published_at = COALESCE($4, published_at),
			updated_at = $5
		WHERE id = $6
	`,
		string(status),
		externalVideoID,
		nullTime(scheduledAt),
		nullTime(publishedAt),
		updatedAt.UTC(),
		id.String(),
	)
	if err != nil {
		return domain.Publication{}, mapPGError("postgres: update publication", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Publication{}, fmt.Errorf("postgres: update publication: %w", ErrNotFound)
	}

	if err := insertAuditEvent(qCtx, tx, "publication", id.String(), "status_updated", map[string]any{
		"status":            status,
		"external_video_id": externalVideoID,
	}); err != nil {
		return domain.Publication{}, err
	}

	pub, err := scanPublication(tx.QueryRow(qCtx, publicationSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return domain.Publication{}, mapPGError("postgres: reload publication", err)
	}
	if err := tx.Commit(qCtx); err != nil {
		return domain.Publication{}, fmt.Errorf("postgres: commit update publication tx: %w", err)
	}
	return pub, nil
}

const publicationSelectSQL = `
	SELECT id, video_job_id, external_video_id, status, idempotency_key,
	       scheduled_at, published_at, created_at
	FROM publications
`

func scanPublication(row pgx.Row) (domain.Publication, error) {
	var (
		id              string
		videoJobID      string
		externalVideoID string
		status          string
		idempotencyKey  string
		scheduledAt     *time.Time
		publishedAt     *time.Time
		createdAt       time.Time
	)
	if err := row.Scan(
		&id, &videoJobID, &externalVideoID, &status, &idempotencyKey,
		&scheduledAt, &publishedAt, &createdAt,
	); err != nil {
		return domain.Publication{}, err
	}
	pub := domain.Publication{
		ID:             domain.PublicationID(id),
		VideoJobID:     domain.VideoJobID(videoJobID),
		YouTubeVideoID: externalVideoID,
		Status:         domain.PublicationStatus(status),
		IdempotencyKey: domain.IdempotencyKey(idempotencyKey),
		CreatedAt:      createdAt.UTC(),
	}
	if scheduledAt != nil {
		pub.ScheduledAt = scheduledAt.UTC()
	}
	if publishedAt != nil {
		pub.PublishedAt = publishedAt.UTC()
	}
	return pub, nil
}
