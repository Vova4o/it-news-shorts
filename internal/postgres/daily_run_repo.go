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

// DailyRunRecord wraps domain.DailyRun with orchestration persistence fields.
type DailyRunRecord struct {
	Run                domain.DailyRun
	WorkflowID         string
	RequestFingerprint string
	StartedAt          time.Time
	FinishedAt         time.Time
	ErrorMessage       string
	Version            int
	UpdatedAt          time.Time
}

// DailyRunRepository persists DailyRun records in PostgreSQL.
type DailyRunRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewDailyRunRepository creates a DailyRunRepository.
func NewDailyRunRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *DailyRunRepository {
	return &DailyRunRepository{pool: pool, timeout: queryTimeout}
}

// CreateIdempotent inserts a DailyRun or returns the existing record with the
// same ID, failing closed with ErrIdempotencyConflict if core parameters differ.
func (r *DailyRunRepository) CreateIdempotent(ctx context.Context, run domain.DailyRun, workflowID string) (DailyRunRecord, bool, error) {
	return r.CreateIdempotentWithFingerprint(ctx, run, workflowID, "")
}

// CreateIdempotentWithFingerprint inserts a DailyRun bound to an immutable
// request fingerprint, or returns the existing record if it matches.
func (r *DailyRunRepository) CreateIdempotentWithFingerprint(ctx context.Context, run domain.DailyRun, workflowID, fingerprint string) (DailyRunRecord, bool, error) {
	if err := run.Validate(); err != nil {
		return DailyRunRecord{}, false, err
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return DailyRunRecord{}, false, fmt.Errorf("postgres: begin daily_run tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	rec, created, err := createDailyRunTx(qCtx, tx, run, workflowID, fingerprint)
	if err != nil {
		return DailyRunRecord{}, false, err
	}
	if err := tx.Commit(qCtx); err != nil {
		return DailyRunRecord{}, false, fmt.Errorf("postgres: commit daily_run tx: %w", err)
	}
	return rec, created, nil
}

// CreateSmokeGraphIdempotent atomically creates DailyRun, Story, and VideoJob
// in one transaction so permanent failures never leave a half-created graph.
func (r *DailyRunRepository) CreateSmokeGraphIdempotent(
	ctx context.Context,
	run domain.DailyRun,
	story domain.Story,
	job domain.VideoJobSnapshot,
	workflowID, fingerprint string,
) (DailyRunRecord, bool, error) {
	if err := run.Validate(); err != nil {
		return DailyRunRecord{}, false, err
	}
	if err := story.Validate(); err != nil {
		return DailyRunRecord{}, false, err
	}
	if _, err := domain.RehydrateVideoJob(job); err != nil {
		return DailyRunRecord{}, false, err
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return DailyRunRecord{}, false, fmt.Errorf("postgres: begin smoke graph tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	rec, created, err := createDailyRunTx(qCtx, tx, run, workflowID, fingerprint)
	if err != nil {
		return DailyRunRecord{}, false, err
	}

	if created {
		if _, err := tx.Exec(qCtx, `
			INSERT INTO stories (id, daily_run_id, title, summary, score, position, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'SMOKE', $7, $7)
			ON CONFLICT (id) DO NOTHING
		`, story.ID.String(), story.DailyRunID.String(), story.Title, story.Summary, story.Score, story.Position, story.CreatedAt.UTC()); err != nil {
			return DailyRunRecord{}, false, mapPGError("postgres: insert smoke story", err)
		}
		valJSON, err := marshalValidation(job.Validation)
		if err != nil {
			return DailyRunRecord{}, false, err
		}
		if _, err := tx.Exec(qCtx, `
			INSERT INTO video_jobs (
				id, story_id, daily_run_id, status, resume_state, attempts, max_attempts,
				idempotency_key, workflow_id, validation, publication_id,
				validated_at, uploaded_at, scheduled_at, published_at,
				created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16)
			ON CONFLICT (idempotency_key) DO NOTHING
		`,
			job.ID.String(),
			job.StoryID.String(),
			job.DailyRunID.String(),
			job.State.String(),
			job.ResumeState.String(),
			job.Attempts,
			job.MaxAttempts,
			job.IdempotencyKey.String(),
			workflowID,
			valJSON,
			job.PublicationID.String(),
			nullTime(job.ValidatedAt),
			nullTime(job.UploadedAt),
			nullTime(job.ScheduledAt),
			nullTime(job.PublishedAt),
			job.CreatedAt.UTC(),
		); err != nil {
			return DailyRunRecord{}, false, mapPGError("postgres: insert smoke video_job", err)
		}
	}

	if err := tx.Commit(qCtx); err != nil {
		return DailyRunRecord{}, false, fmt.Errorf("postgres: commit smoke graph tx: %w", err)
	}
	return rec, created, nil
}

func createDailyRunTx(ctx context.Context, tx pgx.Tx, run domain.DailyRun, workflowID, fingerprint string) (DailyRunRecord, bool, error) {
	var startedAt *time.Time
	if run.State == domain.DailyRunRunning {
		t := run.CreatedAt.UTC()
		startedAt = &t
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO daily_runs (
			id, run_date, window_start, window_end, status, max_stories,
			workflow_id, request_fingerprint, started_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		ON CONFLICT (id) DO NOTHING
	`,
		run.ID.String(),
		run.RunDate.UTC(),
		run.Window.Start.UTC(),
		run.Window.End.UTC(),
		string(run.State),
		run.MaxStories,
		workflowID,
		fingerprint,
		startedAt,
		run.CreatedAt.UTC(),
	)
	if err != nil {
		return DailyRunRecord{}, false, mapPGError("postgres: insert daily_run", err)
	}

	created := tag.RowsAffected() == 1
	if created {
		if err := insertAuditEvent(ctx, tx, "daily_run", run.ID.String(), "created", map[string]any{
			"status":      run.State,
			"workflow_id": workflowID,
		}); err != nil {
			return DailyRunRecord{}, false, err
		}
	}

	rec, err := scanDailyRun(tx.QueryRow(ctx, dailyRunSelectSQL+` WHERE id = $1 FOR UPDATE`, run.ID.String()))
	if err != nil {
		return DailyRunRecord{}, false, mapPGError("postgres: select daily_run", err)
	}

	if !created {
		if fingerprint != "" && rec.RequestFingerprint != "" && rec.RequestFingerprint != fingerprint {
			return DailyRunRecord{}, false, fmt.Errorf(
				"%w: daily_run %s already bound to fingerprint %s (got %s)",
				ErrIdempotencyConflict, run.ID, rec.RequestFingerprint, fingerprint,
			)
		}
		if rec.Run.MaxStories != run.MaxStories ||
			!rec.Run.RunDate.UTC().Equal(run.RunDate.UTC()) ||
			!rec.Run.Window.Start.UTC().Equal(run.Window.Start.UTC()) ||
			!rec.Run.Window.End.UTC().Equal(run.Window.End.UTC()) {
			return DailyRunRecord{}, false, fmt.Errorf(
				"%w: daily_run %s parameters differ from stored run",
				ErrIdempotencyConflict, run.ID,
			)
		}
	}
	return rec, created, nil
}

// MarkFailedIfOwned transitions an active RUNNING DailyRun to FAILED only if
// workflowID owns it, protecting already-completed or foreign runs.
func (r *DailyRunRepository) MarkFailedIfOwned(ctx context.Context, id domain.DailyRunID, workflowID, errMsg string) error {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	now := time.Now().UTC()
	tag, err := r.pool.Exec(qCtx, `
		UPDATE daily_runs
		SET status = 'FAILED',
		    finished_at = $3,
		    error_message = $4,
		    version = version + 1,
		    updated_at = $3
		WHERE id = $1 AND workflow_id = $2 AND status = 'RUNNING'
	`, id.String(), workflowID, now, errMsg)
	if err != nil {
		return mapPGError("postgres: mark daily_run failed", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

// GetByID fetches a DailyRun by primary key.
func (r *DailyRunRepository) GetByID(ctx context.Context, id domain.DailyRunID) (DailyRunRecord, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rec, err := scanDailyRun(r.pool.QueryRow(qCtx, dailyRunSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return DailyRunRecord{}, mapPGError("postgres: get daily_run by id", err)
	}
	return rec, nil
}

// GetByWorkflowID fetches the most recent DailyRun associated with workflowID.
func (r *DailyRunRepository) GetByWorkflowID(ctx context.Context, workflowID string) (DailyRunRecord, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rec, err := scanDailyRun(r.pool.QueryRow(qCtx, dailyRunSelectSQL+` WHERE workflow_id = $1 ORDER BY created_at DESC LIMIT 1`, workflowID))
	if err != nil {
		return DailyRunRecord{}, mapPGError("postgres: get daily_run by workflow_id", err)
	}
	return rec, nil
}

// UpdateStatus transitions a DailyRun state transactionally with optimistic
// concurrency checking.
func (r *DailyRunRepository) UpdateStatus(ctx context.Context, id domain.DailyRunID, state domain.DailyRunState, workflowID, errMsg string, at time.Time) (DailyRunRecord, error) {
	if !state.Valid() {
		return DailyRunRecord{}, domain.InvalidValue("daily_run.state")
	}
	if at.IsZero() {
		return DailyRunRecord{}, domain.MissingField("daily_run.updated_at")
	}
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return DailyRunRecord{}, fmt.Errorf("postgres: begin update daily_run tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	current, err := scanDailyRun(tx.QueryRow(qCtx, dailyRunSelectSQL+` WHERE id = $1 FOR UPDATE`, id.String()))
	if err != nil {
		return DailyRunRecord{}, mapPGError("postgres: lock daily_run", err)
	}

	if current.Run.State.Terminal() && current.Run.State != state {
		return DailyRunRecord{}, fmt.Errorf("%w: daily_run %s is terminal (%s)", domain.ErrInvalidTransition, id, current.Run.State)
	}

	atUTC := at.UTC()
	var startedAt *time.Time
	if !current.StartedAt.IsZero() {
		t := current.StartedAt.UTC()
		startedAt = &t
	} else if state == domain.DailyRunRunning || state == domain.DailyRunCompleted {
		startedAt = &atUTC
	}

	var finishedAt *time.Time
	if state.Terminal() {
		finishedAt = &atUTC
	}

	wfID := current.WorkflowID
	if workflowID != "" {
		wfID = workflowID
	}

	tag, err := tx.Exec(qCtx, `
		UPDATE daily_runs
		SET status = $1,
			workflow_id = $2,
			started_at = $3,
			finished_at = $4,
			error_message = $5,
			version = version + 1,
			updated_at = $6
		WHERE id = $7 AND version = $8
	`,
		string(state),
		wfID,
		startedAt,
		finishedAt,
		errMsg,
		atUTC,
		id.String(),
		current.Version,
	)
	if err != nil {
		return DailyRunRecord{}, mapPGError("postgres: update daily_run", err)
	}
	if tag.RowsAffected() == 0 {
		return DailyRunRecord{}, ErrConcurrentModification
	}

	if err := insertAuditEvent(qCtx, tx, "daily_run", id.String(), "status_updated", map[string]any{
		"from":        current.Run.State,
		"to":          state,
		"workflow_id": wfID,
		"error":       errMsg,
	}); err != nil {
		return DailyRunRecord{}, err
	}

	updated, err := scanDailyRun(tx.QueryRow(qCtx, dailyRunSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return DailyRunRecord{}, mapPGError("postgres: reload daily_run", err)
	}
	if err := tx.Commit(qCtx); err != nil {
		return DailyRunRecord{}, fmt.Errorf("postgres: commit update daily_run tx: %w", err)
	}
	return updated, nil
}

// ListIncomplete returns all runs that are not in a terminal state.
func (r *DailyRunRepository) ListIncomplete(ctx context.Context) ([]DailyRunRecord, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(qCtx, dailyRunSelectSQL+` WHERE status IN ('PENDING', 'RUNNING') ORDER BY created_at ASC`)
	if err != nil {
		return nil, mapPGError("postgres: list incomplete daily_runs", err)
	}
	defer rows.Close()

	var result []DailyRunRecord
	for rows.Next() {
		rec, err := scanDailyRun(rows)
		if err != nil {
			return nil, mapPGError("postgres: scan daily_run", err)
		}
		result = append(result, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate daily_runs", err)
	}
	return result, nil
}

const dailyRunSelectSQL = `
	SELECT id, run_date, window_start, window_end, status, max_stories,
	       workflow_id, request_fingerprint, started_at, finished_at, error_message, version,
	       created_at, updated_at
	FROM daily_runs
`

func scanDailyRun(row pgx.Row) (DailyRunRecord, error) {
	var (
		id                 string
		runDate            time.Time
		winStart           time.Time
		winEnd             time.Time
		status             string
		maxStories         int
		workflowID         string
		requestFingerprint string
		startedAt          *time.Time
		finishedAt         *time.Time
		errMsg             string
		version            int
		createdAt          time.Time
		updatedAt          time.Time
	)
	if err := row.Scan(
		&id, &runDate, &winStart, &winEnd, &status, &maxStories,
		&workflowID, &requestFingerprint, &startedAt, &finishedAt, &errMsg, &version,
		&createdAt, &updatedAt,
	); err != nil {
		return DailyRunRecord{}, err
	}

	rec := DailyRunRecord{
		Run: domain.DailyRun{
			ID:      domain.DailyRunID(id),
			RunDate: runDate.UTC(),
			Window: domain.TimeWindow{
				Start: winStart.UTC(),
				End:   winEnd.UTC(),
			},
			State:      domain.DailyRunState(status),
			MaxStories: maxStories,
			CreatedAt:  createdAt.UTC(),
		},
		WorkflowID:         workflowID,
		RequestFingerprint: requestFingerprint,
		ErrorMessage:       errMsg,
		Version:            version,
		UpdatedAt:          updatedAt.UTC(),
	}
	if startedAt != nil {
		rec.StartedAt = startedAt.UTC()
	}
	if finishedAt != nil {
		rec.FinishedAt = finishedAt.UTC()
	}
	return rec, nil
}

func insertAuditEvent(ctx context.Context, tx pgx.Tx, entityType, entityID, action string, meta map[string]any) error {
	raw, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("postgres: marshal audit metadata: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_events (entity_type, entity_id, action, metadata)
		VALUES ($1, $2, $3, $4)
	`, entityType, entityID, action, raw)
	if err != nil {
		return mapPGError("postgres: insert audit_event", err)
	}
	return nil
}
