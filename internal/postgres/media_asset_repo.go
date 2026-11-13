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

// MediaAssetRepository persists MediaAsset metadata in PostgreSQL.
type MediaAssetRepository struct {
	pool    *pgxpool.Pool
	timeout time.Duration
}

// NewMediaAssetRepository creates a MediaAssetRepository.
func NewMediaAssetRepository(pool *pgxpool.Pool, queryTimeout time.Duration) *MediaAssetRepository {
	return &MediaAssetRepository{pool: pool, timeout: queryTimeout}
}

// CreateIdempotent inserts MediaAsset metadata or returns the existing record
// for the same (bucket, object_key). Metadata must only be persisted after the
// object upload and verification in object storage succeed.
func (r *MediaAssetRepository) CreateIdempotent(ctx context.Context, asset domain.MediaAsset, bucket, objectKey string) (domain.MediaAsset, bool, error) {
	if err := asset.Validate(); err != nil {
		return domain.MediaAsset{}, false, err
	}
	if strings.TrimSpace(bucket) == "" {
		return domain.MediaAsset{}, false, domain.MissingField("media_asset.bucket")
	}
	if strings.TrimSpace(objectKey) == "" {
		return domain.MediaAsset{}, false, domain.MissingField("media_asset.object_key")
	}

	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	tx, err := r.pool.BeginTx(qCtx, pgx.TxOptions{})
	if err != nil {
		return domain.MediaAsset{}, false, fmt.Errorf("postgres: begin media_asset tx: %w", err)
	}
	defer func() { _ = tx.Rollback(qCtx) }()

	tag, err := tx.Exec(qCtx, `
		INSERT INTO media_assets (
			id, video_job_id, asset_type, bucket, object_key, uri,
			content_type, size_bytes, duration_ms, checksum, provider, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (bucket, object_key) DO NOTHING
	`,
		asset.ID.String(),
		asset.VideoJobID.String(),
		string(asset.Kind),
		bucket,
		objectKey,
		asset.URI,
		asset.MIMEType,
		asset.SizeBytes,
		asset.Duration.Milliseconds(),
		asset.Checksum,
		asset.Provider,
		asset.CreatedAt.UTC(),
	)
	if err != nil {
		return domain.MediaAsset{}, false, mapPGError("postgres: insert media_asset", err)
	}

	created := tag.RowsAffected() == 1
	if created {
		if err := insertAuditEvent(qCtx, tx, "media_asset", asset.ID.String(), "created", map[string]any{
			"video_job_id": asset.VideoJobID,
			"bucket":       bucket,
			"object_key":   objectKey,
			"size_bytes":   asset.SizeBytes,
			"checksum":     asset.Checksum,
		}); err != nil {
			return domain.MediaAsset{}, false, err
		}
	}

	stored, err := scanMediaAsset(tx.QueryRow(qCtx, mediaAssetSelectSQL+` WHERE bucket = $1 AND object_key = $2`, bucket, objectKey))
	if err != nil {
		return domain.MediaAsset{}, false, mapPGError("postgres: reload media_asset", err)
	}
	if !created {
		if stored.VideoJobID != asset.VideoJobID ||
			stored.Kind != asset.Kind ||
			stored.Checksum != asset.Checksum ||
			stored.SizeBytes != asset.SizeBytes {
			return domain.MediaAsset{}, false, fmt.Errorf(
				"%w: media_asset %s/%s already committed with checksum=%s size=%d job=%s kind=%s (got checksum=%s size=%d job=%s kind=%s)",
				ErrIdempotencyConflict, bucket, objectKey,
				stored.Checksum, stored.SizeBytes, stored.VideoJobID, stored.Kind,
				asset.Checksum, asset.SizeBytes, asset.VideoJobID, asset.Kind,
			)
		}
	}
	if err := tx.Commit(qCtx); err != nil {
		return domain.MediaAsset{}, false, fmt.Errorf("postgres: commit media_asset tx: %w", err)
	}
	return stored, created, nil
}

// GetByID fetches a MediaAsset by ID.
func (r *MediaAssetRepository) GetByID(ctx context.Context, id domain.MediaAssetID) (domain.MediaAsset, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	asset, err := scanMediaAsset(r.pool.QueryRow(qCtx, mediaAssetSelectSQL+` WHERE id = $1`, id.String()))
	if err != nil {
		return domain.MediaAsset{}, mapPGError("postgres: get media_asset by id", err)
	}
	return asset, nil
}

// GetByObjectKey fetches a MediaAsset by (bucket, objectKey).
func (r *MediaAssetRepository) GetByObjectKey(ctx context.Context, bucket, objectKey string) (domain.MediaAsset, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	asset, err := scanMediaAsset(r.pool.QueryRow(qCtx, mediaAssetSelectSQL+` WHERE bucket = $1 AND object_key = $2`, bucket, objectKey))
	if err != nil {
		return domain.MediaAsset{}, mapPGError("postgres: get media_asset by object_key", err)
	}
	return asset, nil
}

// ObjectExistsInDB reports whether an object key has a recorded MediaAsset row.
func (r *MediaAssetRepository) ObjectExistsInDB(ctx context.Context, bucket, objectKey string) (bool, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	var exists bool
	err := r.pool.QueryRow(qCtx, `SELECT EXISTS(SELECT 1 FROM media_assets WHERE bucket = $1 AND object_key = $2)`, bucket, objectKey).Scan(&exists)
	if err != nil {
		return false, mapPGError("postgres: check media_asset exists", err)
	}
	return exists, nil
}

// ListByVideoJob returns all MediaAsset rows for a VideoJob.
func (r *MediaAssetRepository) ListByVideoJob(ctx context.Context, jobID domain.VideoJobID) ([]domain.MediaAsset, error) {
	qCtx, cancel := withTimeout(ctx, r.timeout)
	defer cancel()

	rows, err := r.pool.Query(qCtx, mediaAssetSelectSQL+` WHERE video_job_id = $1 ORDER BY created_at ASC`, jobID.String())
	if err != nil {
		return nil, mapPGError("postgres: list media_assets", err)
	}
	defer rows.Close()

	var assets []domain.MediaAsset
	for rows.Next() {
		asset, err := scanMediaAsset(rows)
		if err != nil {
			return nil, mapPGError("postgres: scan media_asset", err)
		}
		assets = append(assets, asset)
	}
	if err := rows.Err(); err != nil {
		return nil, mapPGError("postgres: iterate media_assets", err)
	}
	return assets, nil
}

const mediaAssetSelectSQL = `
	SELECT id, video_job_id, asset_type, uri, content_type,
	       size_bytes, duration_ms, checksum, provider, created_at
	FROM media_assets
`

func scanMediaAsset(row pgx.Row) (domain.MediaAsset, error) {
	var (
		id         string
		videoJobID string
		assetType  string
		uri        string
		mimeType   string
		sizeBytes  int64
		durationMS int64
		checksum   string
		provider   string
		createdAt  time.Time
	)
	if err := row.Scan(
		&id, &videoJobID, &assetType, &uri, &mimeType,
		&sizeBytes, &durationMS, &checksum, &provider, &createdAt,
	); err != nil {
		return domain.MediaAsset{}, err
	}
	return domain.MediaAsset{
		ID:         domain.MediaAssetID(id),
		VideoJobID: domain.VideoJobID(videoJobID),
		Kind:       domain.MediaKind(assetType),
		URI:        uri,
		MIMEType:   mimeType,
		SizeBytes:  sizeBytes,
		Duration:   time.Duration(durationMS) * time.Millisecond,
		Checksum:   checksum,
		Provider:   provider,
		CreatedAt:  createdAt.UTC(),
	}, nil
}
