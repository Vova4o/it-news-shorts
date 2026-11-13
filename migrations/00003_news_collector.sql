-- +goose Up
-- +goose StatementBegin

ALTER TABLE articles
    ALTER COLUMN published_at DROP NOT NULL;

DO $$
DECLARE
    con_name TEXT;
BEGIN
    SELECT c.conname INTO con_name
    FROM pg_constraint c
    JOIN pg_class t ON c.conrelid = t.oid
    WHERE t.relname = 'articles'
      AND c.contype = 'c'
      AND pg_get_constraintdef(c.oid) LIKE '%fetched_at >= published_at%';
    IF con_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE articles DROP CONSTRAINT %I', con_name);
    END IF;
END $$;

ALTER TABLE articles
    ADD CONSTRAINT chk_articles_fetched_ge_published
    CHECK (published_at IS NULL OR fetched_at >= published_at);

ALTER TABLE articles
    ADD COLUMN IF NOT EXISTS original_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS canonical_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS normalized_title TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_type TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS author TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS categories JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS popularity_score INTEGER,
    ADD COLUMN IF NOT EXISTS comments_count INTEGER,
    ADD COLUMN IF NOT EXISTS source_submitted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS discovered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS raw_metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE UNIQUE INDEX IF NOT EXISTS idx_articles_source_external_id
    ON articles (source_id, external_id)
    WHERE external_id <> '';

CREATE UNIQUE INDEX IF NOT EXISTS idx_articles_canonical_url
    ON articles (canonical_url)
    WHERE canonical_url <> '';

CREATE INDEX IF NOT EXISTS idx_articles_published_at ON articles (published_at DESC);
CREATE INDEX IF NOT EXISTS idx_articles_fetched_at ON articles (fetched_at DESC);
CREATE INDEX IF NOT EXISTS idx_articles_source_submitted_at ON articles (source_submitted_at DESC);

CREATE TABLE IF NOT EXISTS source_fetch_state (
    source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
    etag TEXT NOT NULL DEFAULT '',
    last_modified TEXT NOT NULL DEFAULT '',
    last_fetch_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS source_fetch_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    workflow_id TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('SUCCESS', 'NOT_MODIFIED', 'ERROR')),
    fetched_count INTEGER NOT NULL DEFAULT 0 CHECK (fetched_count >= 0),
    inserted_count INTEGER NOT NULL DEFAULT 0 CHECK (inserted_count >= 0),
    duplicate_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_count >= 0),
    error_message TEXT NOT NULL DEFAULT '',
    duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_source_fetch_history_source_created
    ON source_fetch_history (source_id, created_at DESC);

CREATE TABLE IF NOT EXISTS article_sources (
    article_id TEXT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    external_id TEXT NOT NULL DEFAULT '',
    original_url TEXT NOT NULL DEFAULT '',
    popularity_score INTEGER,
    comments_count INTEGER,
    source_submitted_at TIMESTAMPTZ,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (article_id, source_id)
);

CREATE INDEX IF NOT EXISTS idx_article_sources_source_id ON article_sources (source_id);

CREATE TABLE IF NOT EXISTS daily_snapshots (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    snapshot_date DATE NOT NULL UNIQUE,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    coverage_status TEXT NOT NULL CHECK (coverage_status IN ('FULL_COVERAGE', 'PARTIAL_COVERAGE')),
    article_count INTEGER NOT NULL DEFAULT 0 CHECK (article_count >= 0),
    source_stats JSONB NOT NULL DEFAULT '{}'::jsonb,
    workflow_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (window_end > window_start)
);

CREATE INDEX IF NOT EXISTS idx_daily_snapshots_window ON daily_snapshots (window_start, window_end);

CREATE TABLE IF NOT EXISTS daily_snapshot_entries (
    snapshot_id TEXT NOT NULL REFERENCES daily_snapshots(id) ON DELETE CASCADE,
    article_id TEXT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    position INTEGER NOT NULL DEFAULT 0 CHECK (position >= 0),
    PRIMARY KEY (snapshot_id, article_id)
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS daily_snapshot_entries;
DROP TABLE IF EXISTS daily_snapshots;
DROP TABLE IF EXISTS article_sources;
DROP TABLE IF EXISTS source_fetch_history;
DROP TABLE IF EXISTS source_fetch_state;

DROP INDEX IF EXISTS idx_articles_source_submitted_at;
DROP INDEX IF EXISTS idx_articles_fetched_at;
DROP INDEX IF EXISTS idx_articles_published_at;
DROP INDEX IF EXISTS idx_articles_canonical_url;
DROP INDEX IF EXISTS idx_articles_source_external_id;

ALTER TABLE articles
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS raw_metadata,
    DROP COLUMN IF EXISTS discovered_at,
    DROP COLUMN IF EXISTS source_submitted_at,
    DROP COLUMN IF EXISTS comments_count,
    DROP COLUMN IF EXISTS popularity_score,
    DROP COLUMN IF EXISTS categories,
    DROP COLUMN IF EXISTS author,
    DROP COLUMN IF EXISTS source_type,
    DROP COLUMN IF EXISTS normalized_title,
    DROP COLUMN IF EXISTS canonical_url,
    DROP COLUMN IF EXISTS original_url;

ALTER TABLE articles
    DROP CONSTRAINT IF EXISTS chk_articles_fetched_ge_published;

-- +goose StatementEnd
