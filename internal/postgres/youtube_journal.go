package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/youtube"
)

// ErrLockLost is returned when an advisory lock holder lost its lock connection
// or was fenced out by a newer owner.
var ErrLockLost = errors.New("postgres: advisory lock ownership lost")

type activeJournalLock struct {
	token int64
	conn  *pgx.Conn
}

// PGJournal implements youtube.UploadJournal backed by PostgreSQL.
//
// Locking architecture:
//   - Same-process callers for the same idempotency key serialize on an
//     in-process per-key semaphore (cancellable via ctx).
//   - Cross-process callers serialize on PostgreSQL session advisory locks
//     using a dedicated connection outside queryPool so lock holders never
//     starve Get/Put/FindByVideoID calls on queryPool.
//   - Each acquired lock bumps a monotonic fencing_token in youtube_journal_locks
//     and checks lock connection liveness + fencing_token on mutating operations
//     to prevent stale owners from overwriting newer state after connection loss.
type PGJournal struct {
	queryPool *pgxpool.Pool
	timeout   time.Duration

	mu     sync.Mutex
	locks  map[string]chan struct{}
	active map[string]*activeJournalLock
}

// NewPGJournal creates a PostgreSQL-backed YouTube upload journal.
func NewPGJournal(queryPool *pgxpool.Pool, queryTimeout time.Duration) *PGJournal {
	return &PGJournal{
		queryPool: queryPool,
		timeout:   queryTimeout,
		locks:     make(map[string]chan struct{}),
		active:    make(map[string]*activeJournalLock),
	}
}

// AcquireKey acquires an exclusive per-key lock (in-process + PostgreSQL
// advisory lock) respecting ctx cancellation.
func (j *PGJournal) AcquireKey(ctx context.Context, key string) (func(), error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("postgres: journal entry key is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sem := j.keySemaphore(key)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	releaseInProc := func() { <-sem }

	connCfg := j.queryPool.Config().ConnConfig.Copy()
	lockConn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		releaseInProc()
		return nil, fmt.Errorf("postgres: open advisory lock connection: %w", err)
	}

	for {
		if err := ctx.Err(); err != nil {
			_ = lockConn.Close(context.Background())
			releaseInProc()
			return nil, err
		}

		qCtx, cancel := withTimeout(ctx, j.timeout)
		var acquired bool
		err := lockConn.QueryRow(qCtx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, key).Scan(&acquired)
		cancel()
		if err != nil {
			_ = lockConn.Close(context.Background())
			releaseInProc()
			return nil, fmt.Errorf("postgres: try advisory lock: %w", err)
		}
		if acquired {
			fCtx, cancelFence := withTimeout(ctx, j.timeout)
			var token int64
			err := lockConn.QueryRow(fCtx, `
				INSERT INTO youtube_journal_locks (idempotency_key, fencing_token, updated_at)
				VALUES ($1, 1, NOW())
				ON CONFLICT (idempotency_key) DO UPDATE
				SET fencing_token = youtube_journal_locks.fencing_token + 1,
				    updated_at = NOW()
				RETURNING fencing_token
			`, key).Scan(&token)
			cancelFence()
			if err != nil {
				_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
				_ = lockConn.Close(context.Background())
				releaseInProc()
				return nil, fmt.Errorf("postgres: acquire fencing token: %w", err)
			}

			holder := &activeJournalLock{token: token, conn: lockConn}
			j.mu.Lock()
			j.active[key] = holder
			j.mu.Unlock()

			return func() {
				j.mu.Lock()
				if j.active[key] == holder {
					delete(j.active, key)
				}
				j.mu.Unlock()

				unlockCtx, cancelUnlock := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancelUnlock()
				_, _ = lockConn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key)
				_ = lockConn.Close(unlockCtx)
				releaseInProc()
			}, nil
		}

		timer := time.NewTimer(15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = lockConn.Close(context.Background())
			releaseInProc()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (j *PGJournal) keySemaphore(key string) chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	ch, ok := j.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		j.locks[key] = ch
	}
	return ch
}

func (j *PGJournal) verifyActiveLock(ctx context.Context, key string) (int64, bool, error) {
	j.mu.Lock()
	holder := j.active[key]
	j.mu.Unlock()
	if holder == nil {
		return 0, false, nil
	}

	pCtx, cancel := withTimeout(ctx, j.timeout)
	defer cancel()
	var ok int
	if err := holder.conn.QueryRow(pCtx, `SELECT 1`).Scan(&ok); err != nil {
		return 0, false, fmt.Errorf("%w: key %q lock connection closed: %v", ErrLockLost, key, err)
	}
	return holder.token, true, nil
}

// Get returns the journal entry for key.
func (j *PGJournal) Get(ctx context.Context, key string) (youtube.JournalEntry, bool, error) {
	if _, _, err := j.verifyActiveLock(ctx, key); err != nil {
		return youtube.JournalEntry{}, false, err
	}

	qCtx, cancel := withTimeout(ctx, j.timeout)
	defer cancel()

	entry, err := scanJournalEntry(j.queryPool.QueryRow(qCtx, journalSelectSQL+` WHERE idempotency_key = $1`, key))
	if err != nil {
		if err == pgx.ErrNoRows {
			return youtube.JournalEntry{}, false, nil
		}
		return youtube.JournalEntry{}, false, mapPGError("postgres: get youtube journal entry", err)
	}
	return entry, true, nil
}

// FindByVideoID returns the first journal entry matching videoID.
func (j *PGJournal) FindByVideoID(ctx context.Context, videoID string) (youtube.JournalEntry, bool, error) {
	qCtx, cancel := withTimeout(ctx, j.timeout)
	defer cancel()

	entry, err := scanJournalEntry(j.queryPool.QueryRow(qCtx, journalSelectSQL+` WHERE video_id = $1 ORDER BY created_at ASC LIMIT 1`, videoID))
	if err != nil {
		if err == pgx.ErrNoRows {
			return youtube.JournalEntry{}, false, nil
		}
		return youtube.JournalEntry{}, false, mapPGError("postgres: find youtube journal by video_id", err)
	}
	return entry, true, nil
}

// Put stores or updates a journal entry, preserving original CreatedAt when
// already present. Enforces all YouTube journal invariants and fencing-token
// checks when called under AcquireKey.
func (j *PGJournal) Put(ctx context.Context, entry youtube.JournalEntry) error {
	if err := youtube.ValidateJournalEntry(entry); err != nil {
		return err
	}
	expectedToken, hasLock, err := j.verifyActiveLock(ctx, entry.Key)
	if err != nil {
		return err
	}

	qCtx, cancel := withTimeout(ctx, j.timeout)
	defer cancel()

	now := time.Now().UTC()
	createdAt := entry.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := entry.UpdatedAt.UTC()
	if updatedAt.IsZero() {
		updatedAt = now
	}

	const upsertSQL = `
		INSERT INTO youtube_upload_journal (
			idempotency_key, state, video_id, upload_status, session_uri,
			total_bytes, sent_bytes, file_sha256, title, privacy,
			video_job_id, publication_id, error_message, uploaded_at,
			created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (idempotency_key) DO UPDATE SET
			state = EXCLUDED.state,
			video_id = EXCLUDED.video_id,
			upload_status = EXCLUDED.upload_status,
			session_uri = EXCLUDED.session_uri,
			total_bytes = EXCLUDED.total_bytes,
			sent_bytes = EXCLUDED.sent_bytes,
			file_sha256 = EXCLUDED.file_sha256,
			title = EXCLUDED.title,
			privacy = EXCLUDED.privacy,
			video_job_id = EXCLUDED.video_job_id,
			publication_id = EXCLUDED.publication_id,
			error_message = EXCLUDED.error_message,
			uploaded_at = EXCLUDED.uploaded_at,
			updated_at = EXCLUDED.updated_at
	`

	args := []any{
		entry.Key,
		string(entry.State),
		entry.VideoID,
		entry.UploadStatus,
		entry.SessionURI,
		entry.TotalBytes,
		entry.SentBytes,
		entry.FileSHA256,
		entry.Title,
		entry.Privacy,
		entry.VideoJobID,
		entry.PublicationID,
		entry.Error,
		nullTime(entry.UploadedAt),
		createdAt,
		updatedAt,
	}

	if !hasLock {
		if _, err := j.queryPool.Exec(qCtx, upsertSQL, args...); err != nil {
			return mapPGError("postgres: put youtube journal entry", err)
		}
		return nil
	}

	tx, err := j.queryPool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return mapPGError("postgres: begin fenced journal put tx", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var currentToken int64
	if err := tx.QueryRow(qCtx, `SELECT fencing_token FROM youtube_journal_locks WHERE idempotency_key = $1 FOR SHARE`, entry.Key).Scan(&currentToken); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: missing fencing row for key %q", ErrLockLost, entry.Key)
		}
		return mapPGError("postgres: verify fencing token", err)
	}
	if currentToken != expectedToken {
		return fmt.Errorf("%w: key %q fenced out (held=%d current=%d)", ErrLockLost, entry.Key, expectedToken, currentToken)
	}

	if _, err := tx.Exec(qCtx, upsertSQL, args...); err != nil {
		return mapPGError("postgres: put youtube journal entry", err)
	}
	if err := tx.Commit(qCtx); err != nil {
		return mapPGError("postgres: commit fenced journal put", err)
	}
	return nil
}

// List returns all journal entries ordered by creation time, newest first.
func (j *PGJournal) List(ctx context.Context) ([]youtube.JournalEntry, error) {
	qCtx, cancel := withTimeout(ctx, j.timeout)
	defer cancel()

	rows, err := j.queryPool.Query(qCtx, journalSelectSQL+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, mapPGError("postgres: list youtube journal entries", err)
	}
	defer rows.Close()

	var entries []youtube.JournalEntry
	for rows.Next() {
		entry, err := scanJournalEntry(rows)
		if err != nil {
			return nil, mapPGError("postgres: scan youtube journal entry", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate youtube journal entries", err)
	}
	return entries, nil
}

// JournalMigrationReport summarizes a file-to-PostgreSQL journal import.
type JournalMigrationReport struct {
	Total    int
	Imported int
	Skipped  int
}

// MigrateFileJournalToPG idempotently imports all entries from a file-based
// youtube.Journal into PostgreSQL. It fails closed on conflicting entries and
// preserves UNKNOWN, IN_PROGRESS, PENDING, FAILED, and COMPLETED states.
func MigrateFileJournalToPG(ctx context.Context, fileJournal *youtube.Journal, pgJournal *PGJournal) (JournalMigrationReport, error) {
	if fileJournal == nil {
		return JournalMigrationReport{}, fmt.Errorf("postgres: file journal is nil")
	}
	if pgJournal == nil {
		return JournalMigrationReport{}, fmt.Errorf("postgres: pg journal is nil")
	}

	entries, err := fileJournal.List(ctx)
	if err != nil {
		return JournalMigrationReport{}, fmt.Errorf("postgres: read file journal: %w", err)
	}

	report := JournalMigrationReport{Total: len(entries)}
	for _, entry := range entries {
		if err := youtube.ValidateJournalEntry(entry); err != nil {
			return report, fmt.Errorf("postgres: corrupt journal entry for key %q: %w", entry.Key, err)
		}

		release, err := pgJournal.AcquireKey(ctx, entry.Key)
		if err != nil {
			return report, fmt.Errorf("postgres: lock journal key %q: %w", entry.Key, err)
		}

		existing, found, err := pgJournal.Get(ctx, entry.Key)
		if err != nil {
			release()
			return report, err
		}
		if found {
			if !journalEntriesMatch(existing, entry) {
				release()
				return report, fmt.Errorf(
					"postgres: journal conflict for key %q (pg state=%s video_id=%s sha256=%s vs file state=%s video_id=%s sha256=%s)",
					entry.Key, existing.State, existing.VideoID, existing.FileSHA256,
					entry.State, entry.VideoID, entry.FileSHA256,
				)
			}
			release()
			report.Skipped++
			continue
		}

		if err := pgJournal.Put(ctx, entry); err != nil {
			release()
			return report, fmt.Errorf("postgres: import journal key %q: %w", entry.Key, err)
		}
		release()
		report.Imported++
	}
	return report, nil
}

func journalEntriesMatch(a, b youtube.JournalEntry) bool {
	if a.Key != b.Key ||
		a.State != b.State ||
		a.VideoID != b.VideoID ||
		a.UploadStatus != b.UploadStatus ||
		a.SessionURI != b.SessionURI ||
		a.TotalBytes != b.TotalBytes ||
		a.SentBytes != b.SentBytes ||
		a.FileSHA256 != b.FileSHA256 ||
		a.Title != b.Title ||
		a.Privacy != b.Privacy ||
		a.VideoJobID != b.VideoJobID ||
		a.PublicationID != b.PublicationID ||
		a.Error != b.Error ||
		!timesMatchMicro(a.UploadedAt, b.UploadedAt) {
		return false
	}
	if !b.CreatedAt.IsZero() && !timesMatchMicro(a.CreatedAt, b.CreatedAt) {
		return false
	}
	if !b.UpdatedAt.IsZero() && !timesMatchMicro(a.UpdatedAt, b.UpdatedAt) {
		return false
	}
	return true
}

func timesMatchMicro(a, b time.Time) bool {
	if a.IsZero() && b.IsZero() {
		return true
	}
	if a.IsZero() != b.IsZero() {
		return false
	}
	return a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

const journalSelectSQL = `
	SELECT idempotency_key, state, video_id, upload_status, session_uri,
	       total_bytes, sent_bytes, file_sha256, title, privacy,
	       video_job_id, publication_id, error_message, uploaded_at,
	       created_at, updated_at
	FROM youtube_upload_journal
`

func scanJournalEntry(row pgx.Row) (youtube.JournalEntry, error) {
	var (
		key          string
		state        string
		videoID      string
		uploadStatus string
		sessionURI   string
		totalBytes   int64
		sentBytes    int64
		fileSHA256   string
		title        string
		privacy      string
		videoJobID   string
		pubID        string
		errMsg       string
		uploadedAt   *time.Time
		createdAt    time.Time
		updatedAt    time.Time
	)
	if err := row.Scan(
		&key, &state, &videoID, &uploadStatus, &sessionURI,
		&totalBytes, &sentBytes, &fileSHA256, &title, &privacy,
		&videoJobID, &pubID, &errMsg, &uploadedAt,
		&createdAt, &updatedAt,
	); err != nil {
		return youtube.JournalEntry{}, err
	}
	entry := youtube.JournalEntry{
		Key:           key,
		State:         youtube.UploadState(state),
		VideoID:       videoID,
		UploadStatus:  uploadStatus,
		SessionURI:    sessionURI,
		TotalBytes:    totalBytes,
		SentBytes:     sentBytes,
		FileSHA256:    fileSHA256,
		Title:         title,
		Privacy:       privacy,
		VideoJobID:    videoJobID,
		PublicationID: pubID,
		Error:         errMsg,
		CreatedAt:     createdAt.UTC(),
		UpdatedAt:     updatedAt.UTC(),
	}
	if uploadedAt != nil {
		entry.UploadedAt = uploadedAt.UTC()
	}
	if err := youtube.ValidateJournalEntry(entry); err != nil {
		return youtube.JournalEntry{}, fmt.Errorf("postgres: corrupt stored journal row for key %q: %w", entry.Key, err)
	}
	return entry, nil
}
