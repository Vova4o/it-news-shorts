package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/domain"
)

// VideoJobRepository persists VideoJob aggregates in PostgreSQL and enforces
// domain state machine transitions inside database transactions.
type VideoJobRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewVideoJobRepository creates a VideoJobRepository.
func NewVideoJobRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *VideoJobRepository {
	return &VideoJobRepository{pool: pool, timeout: queryTimeout}
}

// CreateIdempotent inserts a VideoJob or returns the existing job with the same
// idempotency key. If a different job already uses the same primary ID, it
// returns ErrConflict.
func (r *VideoJobRepository) CreateIdempotent(ctx context.Context, job *domain.VideoJob, workflowID string) (*domain.VideoJob, bool, error) {
	if job == nil {
		return nil, false, domain.MissingField("video_job")
	}
	if err := job.Validate(); err != nil {
		return nil, false, err
	}
	snap := job.Snapshot()
	valJSON, err := marshalValidation(snap.Validation)
	if err != nil {
		return nil, false, err
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("postgres: begin video_job tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	tag, err := tx.Exec(qCtx, `
		INSERT INTO video_jobs (
			id, story_id, daily_run_id, status, resume_state, attempts, max_attempts,
			idempotency_key, workflow_id, validation, publication_id,
			validated_at, uploaded_at, scheduled_at, published_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (idempotency_key) DO NOTHING
	`,
		snap.ID.String(),
		snap.StoryID.String(),
		snap.DailyRunID.String(),
		snap.State.String(),
		snap.ResumeState.String(),
		snap.Attempts,
		snap.MaxAttempts,
		snap.IdempotencyKey.String(),
		workflowID,
		valJSON,
		snap.PublicationID.String(),
		nullTime(snap.ValidatedAt),
		nullTime(snap.UploadedAt),
		nullTime(snap.ScheduledAt),
		nullTime(snap.PublishedAt),
		snap.CreatedAt.UTC(),
		snap.UpdatedAt.UTC(),
	)
	if err != nil {
		return nil, false, mapPGError("postgres: insert video_job", err)
	}

	created := tag.RowsAffected() == 1
	if created {
		if err := insertAuditEvent(qCtx, tx, "video_job", snap.ID.String(), "created", map[string]any{
			"status":          snap.State,
			"idempotency_key": snap.IdempotencyKey,
			"workflow_id":     workflowID,
		}); err != nil {
			return nil, false, err
		}
	}

	existing, _, err := scanVideoJob(tx.QueryRow(qCtx, videoJobSelectSQL+` WHERE idempotency_key = $1`, snap.IdempotencyKey.String()))
	if err != nil {
		return nil, false, mapPGError("postgres: reload video_job", err)
	}
	if err := tx.Commit(qCtx); err != nil {
		return nil, false, fmt.Errorf("postgres: commit video_job tx: %w", err)
	}
	return existing, created, nil
}

// GetByID fetches a VideoJob and its optimistic-locking version.
func (r *VideoJobRepository) GetByID(ctx context.Context, id domain.VideoJobID) (*domain.VideoJob, int, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	job, version, err := scanVideoJob(r.pool.QueryRow(qCtx, videoJobSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return nil, 0, mapPGError("postgres: get video_job by id", err)
	}
	return job, version, nil
}

// GetByIdempotencyKey fetches a VideoJob by its idempotency key.
func (r *VideoJobRepository) GetByIdempotencyKey(ctx context.Context, key domain.IdempotencyKey) (*domain.VideoJob, int, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	job, version, err := scanVideoJob(r.pool.QueryRow(qCtx, videoJobSelectSQL+` WHERE idempotency_key = $1`, key.String()))
	if err != nil {
		return nil, 0, mapPGError("postgres: get video_job by key", err)
	}
	return job, version, nil
}

// ListByDailyRun returns all jobs belonging to a DailyRun.
func (r *VideoJobRepository) ListByDailyRun(ctx context.Context, runID domain.DailyRunID) ([]*domain.VideoJob, error) {
	return r.listQuery(ctx, videoJobSelectSQL+` WHERE daily_run_id = $1 ORDER BY created_at ASC`, runID.String())
}

// ListIncomplete returns all jobs that have not reached a terminal state.
func (r *VideoJobRepository) ListIncomplete(ctx context.Context) ([]*domain.VideoJob, error) {
	return r.listQuery(ctx, videoJobSelectSQL+` WHERE status NOT IN ('PUBLISHED', 'REJECTED') ORDER BY created_at ASC`)
}

func (r *VideoJobRepository) listQuery(ctx context.Context, query string, args ...any) ([]*domain.VideoJob, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(qCtx, query, args...)
	if err != nil {
		return nil, mapPGError("postgres: list video_jobs", err)
	}
	defer rows.Close()

	var jobs []*domain.VideoJob
	for rows.Next() {
		job, _, err := scanVideoJob(rows)
		if err != nil {
			return nil, mapPGError("postgres: scan video_job", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate video_jobs", err)
	}
	return jobs, nil
}

// Transition atomically loads, validates via domain.VideoJob.Transition, and
// persists the updated state with an audit event.
func (r *VideoJobRepository) Transition(
	ctx context.Context,
	id domain.VideoJobID,
	to domain.VideoState,
	at time.Time,
	mutate func(*domain.VideoJob) error,
) (*domain.VideoJob, error) {
	return r.transitionInternal(ctx, id, 0, false, to, at, mutate)
}

// TransitionWithExpectedVersion performs the same transactional domain
// transition while also enforcing that the stored row matches expectedVersion.
func (r *VideoJobRepository) TransitionWithExpectedVersion(
	ctx context.Context,
	id domain.VideoJobID,
	expectedVersion int,
	to domain.VideoState,
	at time.Time,
	mutate func(*domain.VideoJob) error,
) (*domain.VideoJob, error) {
	return r.transitionInternal(ctx, id, expectedVersion, true, to, at, mutate)
}

func (r *VideoJobRepository) transitionInternal(
	ctx context.Context,
	id domain.VideoJobID,
	expectedVersion int,
	checkVersion bool,
	to domain.VideoState,
	at time.Time,
	mutate func(*domain.VideoJob) error,
) (*domain.VideoJob, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("postgres: begin video_job transition tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	job, currentVersion, err := scanVideoJob(tx.QueryRow(qCtx, videoJobSelectSQL+` WHERE id = $1 FOR UPDATE`, id.String()))
	if err != nil {
		return nil, mapPGError("postgres: lock video_job", err)
	}
	if checkVersion && currentVersion != expectedVersion {
		return nil, fmt.Errorf("%w: video_job %s version %d != expected %d", ErrConcurrentModification, id, currentVersion, expectedVersion)
	}

	from := job.State()
	if mutate != nil {
		if err := mutate(job); err != nil {
			return nil, err
		}
	}
	if err := job.Transition(to, at); err != nil {
		return nil, err
	}

	snap := job.Snapshot()
	valJSON, err := marshalValidation(snap.Validation)
	if err != nil {
		return nil, err
	}

	tag, err := tx.Exec(qCtx, `
		UPDATE video_jobs
		SET status = $1,
			resume_state = $2,
			attempts = $3,
			validation = $4,
			publication_id = $5,
			validated_at = $6,
			uploaded_at = $7,
			scheduled_at = $8,
			published_at = $9,
			version = version + 1,
			updated_at = $10
		WHERE id = $11 AND version = $12
	`,
		snap.State.String(),
		snap.ResumeState.String(),
		snap.Attempts,
		valJSON,
		snap.PublicationID.String(),
		nullTime(snap.ValidatedAt),
		nullTime(snap.UploadedAt),
		nullTime(snap.ScheduledAt),
		nullTime(snap.PublishedAt),
		snap.UpdatedAt.UTC(),
		snap.ID.String(),
		currentVersion,
	)
	if err != nil {
		return nil, mapPGError("postgres: update video_job", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrConcurrentModification
	}

	if err := insertAuditEvent(qCtx, tx, "video_job", snap.ID.String(), "transition", map[string]any{
		"from":    from,
		"to":      snap.State,
		"attempt": snap.Attempts,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(qCtx); err != nil {
		return nil, fmt.Errorf("postgres: commit video_job transition tx: %w", err)
	}
	return job, nil
}

// AttachPublication binds a PublicationID to a VideoJob inside a transaction
// using domain.VideoJob.AttachPublication.
func (r *VideoJobRepository) AttachPublication(ctx context.Context, id domain.VideoJobID, pubID domain.PublicationID, at time.Time) (*domain.VideoJob, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("postgres: begin attach publication tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	job, version, err := scanVideoJob(tx.QueryRow(qCtx, videoJobSelectSQL+` WHERE id = $1 FOR UPDATE`, id.String()))
	if err != nil {
		return nil, mapPGError("postgres: lock video_job", err)
	}
	if err := job.AttachPublication(pubID, at); err != nil {
		return nil, err
	}

	tag, err := tx.Exec(qCtx, `
		UPDATE video_jobs
		SET publication_id = $1,
			version = version + 1,
			updated_at = $2
		WHERE id = $3 AND version = $4
	`, pubID.String(), at.UTC(), id.String(), version)
	if err != nil {
		return nil, mapPGError("postgres: attach publication", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrConcurrentModification
	}

	if err := insertAuditEvent(qCtx, tx, "video_job", id.String(), "publication_attached", map[string]any{
		"publication_id": pubID,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(qCtx); err != nil {
		return nil, fmt.Errorf("postgres: commit attach publication tx: %w", err)
	}
	return job, nil
}

// RecordError records the latest operational error message on a VideoJob.
func (r *VideoJobRepository) RecordError(ctx context.Context, id domain.VideoJobID, errMsg string, at time.Time) error {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tag, err := r.pool.Exec(qCtx, `
		UPDATE video_jobs
		SET error_message = $1,
			updated_at = $2
		WHERE id = $3
	`, errMsg, at.UTC(), id.String())
	if err != nil {
		return mapPGError("postgres: record video_job error", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: record video_job error: %w", ErrNotFound)
	}
	return nil
}

const videoJobSelectSQL = `
	SELECT id, story_id, daily_run_id, status, resume_state, attempts, max_attempts,
	       idempotency_key, validation, publication_id, version,
	       validated_at, uploaded_at, scheduled_at, published_at,
	       created_at, updated_at
	FROM video_jobs
`

func scanVideoJob(row pgx.Row) (*domain.VideoJob, int, error) {
	var (
		id             string
		storyID        string
		dailyRunID     string
		status         string
		resumeState    string
		attempts       int
		maxAttempts    int
		idempotencyKey string
		valRaw         []byte
		publicationID  string
		version        int
		validatedAt    *time.Time
		uploadedAt     *time.Time
		scheduledAt    *time.Time
		publishedAt    *time.Time
		createdAt      time.Time
		updatedAt      time.Time
	)
	if err := row.Scan(
		&id, &storyID, &dailyRunID, &status, &resumeState, &attempts, &maxAttempts,
		&idempotencyKey, &valRaw, &publicationID, &version,
		&validatedAt, &uploadedAt, &scheduledAt, &publishedAt,
		&createdAt, &updatedAt,
	); err != nil {
		return nil, 0, err
	}

	var validation *domain.VideoValidation
	if len(valRaw) > 0 && string(valRaw) != "null" {
		var v domain.VideoValidation
		if err := json.Unmarshal(valRaw, &v); err != nil {
			return nil, 0, fmt.Errorf("postgres: unmarshal video_job validation: %w", err)
		}
		validation = &v
	}

	snap := domain.VideoJobSnapshot{
		ID:             domain.VideoJobID(id),
		DailyRunID:     domain.DailyRunID(dailyRunID),
		StoryID:        domain.StoryID(storyID),
		IdempotencyKey: domain.IdempotencyKey(idempotencyKey),
		State:          domain.VideoState(status),
		ResumeState:    domain.VideoState(resumeState),
		Attempts:       attempts,
		MaxAttempts:    maxAttempts,
		Validation:     validation,
		PublicationID:  domain.PublicationID(publicationID),
		CreatedAt:      createdAt.UTC(),
		UpdatedAt:      updatedAt.UTC(),
	}
	if validatedAt != nil {
		snap.ValidatedAt = validatedAt.UTC()
	}
	if uploadedAt != nil {
		snap.UploadedAt = uploadedAt.UTC()
	}
	if scheduledAt != nil {
		snap.ScheduledAt = scheduledAt.UTC()
	}
	if publishedAt != nil {
		snap.PublishedAt = publishedAt.UTC()
	}

	job, err := domain.RehydrateVideoJob(snap)
	if err != nil {
		return nil, 0, err
	}
	return job, version, nil
}

func marshalValidation(v *domain.VideoValidation) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshal video_job validation: %w", err)
	}
	return raw, nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	utc := t.UTC()
	return &utc
}
