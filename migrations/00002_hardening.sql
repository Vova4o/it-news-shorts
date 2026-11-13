-- +goose Up
-- +goose StatementBegin

-- 1. Enforce upload journal invariants at the database layer.
ALTER TABLE youtube_upload_journal
    ADD CONSTRAINT chk_yt_journal_bytes_nonneg CHECK (total_bytes >= 0 AND sent_bytes >= 0),
    ADD CONSTRAINT chk_yt_journal_sent_le_total CHECK (total_bytes = 0 OR sent_bytes <= total_bytes),
    ADD CONSTRAINT chk_yt_journal_sha256_hex CHECK (file_sha256 = '' OR file_sha256 ~ '^[0-9a-fA-F]{64}$'),
    ADD CONSTRAINT chk_yt_journal_inprog_session CHECK (state <> 'IN_PROGRESS' OR btrim(session_uri) <> ''),
    ADD CONSTRAINT chk_yt_journal_completed_vid CHECK (state <> 'COMPLETED' OR btrim(video_id) <> '');

-- 2. Monotonic fencing tokens for PGJournal advisory locks.
CREATE TABLE IF NOT EXISTS youtube_journal_locks (
    idempotency_key TEXT PRIMARY KEY,
    fencing_token   BIGINT NOT NULL DEFAULT 0,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 3. Immutable input fingerprint for business-level run idempotency.
ALTER TABLE daily_runs
    ADD COLUMN IF NOT EXISTS request_fingerprint TEXT NOT NULL DEFAULT '';

-- 4. Allow explicit SMOKE_ARTIFACT diagnostic type in media_assets without
-- disguising text smoke artifacts as RENDERED_VIDEO.
DO $$
DECLARE
    con_name TEXT;
BEGIN
    SELECT c.conname INTO con_name
    FROM pg_constraint c
    JOIN pg_class t ON c.conrelid = t.oid
    WHERE t.relname = 'media_assets'
      AND c.contype = 'c'
      AND pg_get_constraintdef(c.oid) LIKE '%asset_type%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE media_assets DROP CONSTRAINT %I', con_name);
    END IF;
END $$;

ALTER TABLE media_assets
    ADD CONSTRAINT chk_media_assets_asset_type
    CHECK (asset_type IN ('VOICEOVER', 'BACKGROUND', 'CLIP', 'RENDERED_VIDEO', 'THUMBNAIL', 'SMOKE_ARTIFACT'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE media_assets
    DROP CONSTRAINT IF EXISTS chk_media_assets_asset_type,
    ADD CONSTRAINT media_assets_asset_type_check
    CHECK (asset_type IN ('VOICEOVER', 'BACKGROUND', 'CLIP', 'RENDERED_VIDEO', 'THUMBNAIL'));

ALTER TABLE daily_runs
    DROP COLUMN IF EXISTS request_fingerprint;

DROP TABLE IF EXISTS youtube_journal_locks;

ALTER TABLE youtube_upload_journal
    DROP CONSTRAINT IF EXISTS chk_yt_journal_completed_vid,
    DROP CONSTRAINT IF EXISTS chk_yt_journal_inprog_session,
    DROP CONSTRAINT IF EXISTS chk_yt_journal_sha256_hex,
    DROP CONSTRAINT IF EXISTS chk_yt_journal_sent_le_total,
    DROP CONSTRAINT IF EXISTS chk_yt_journal_bytes_nonneg;

-- +goose StatementEnd
