-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS sources (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    name TEXT NOT NULL CHECK (btrim(name) <> ''),
    source_type TEXT NOT NULL CHECK (btrim(source_type) <> ''),
    base_url TEXT NOT NULL CHECK (btrim(base_url) <> ''),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS articles (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
    external_id TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL CHECK (btrim(title) <> ''),
    url TEXT NOT NULL UNIQUE CHECK (btrim(url) <> ''),
    summary TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    language TEXT NOT NULL CHECK (language IN ('ru', 'en')),
    published_at TIMESTAMPTZ NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (fetched_at >= published_at)
);

CREATE INDEX IF NOT EXISTS idx_articles_source_published ON articles (source_id, published_at DESC);

CREATE TABLE IF NOT EXISTS daily_runs (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    run_date TIMESTAMPTZ NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'RUNNING', 'COMPLETED', 'FAILED')),
    max_stories INTEGER NOT NULL CHECK (max_stories BETWEEN 1 AND 3),
    workflow_id TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    error_message TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (window_end > window_start)
);

CREATE INDEX IF NOT EXISTS idx_daily_runs_status ON daily_runs (status);
CREATE INDEX IF NOT EXISTS idx_daily_runs_workflow_id ON daily_runs (workflow_id) WHERE workflow_id <> '';
CREATE INDEX IF NOT EXISTS idx_daily_runs_run_date ON daily_runs (run_date DESC);

CREATE TABLE IF NOT EXISTS stories (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    daily_run_id TEXT NOT NULL REFERENCES daily_runs(id) ON DELETE RESTRICT,
    title TEXT NOT NULL CHECK (btrim(title) <> ''),
    summary TEXT NOT NULL DEFAULT '',
    score NUMERIC(6, 5) NOT NULL DEFAULT 0 CHECK (score >= 0 AND score <= 1),
    position INTEGER NOT NULL DEFAULT 0 CHECK (position BETWEEN 0 AND 3),
    status TEXT NOT NULL DEFAULT 'CANDIDATE' CHECK (btrim(status) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_stories_daily_run_id ON stories (daily_run_id);

CREATE TABLE IF NOT EXISTS story_evidence (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    article_id TEXT NOT NULL REFERENCES articles(id) ON DELETE RESTRICT,
    claim TEXT NOT NULL CHECK (btrim(claim) <> ''),
    source TEXT NOT NULL CHECK (btrim(source) <> ''),
    source_url TEXT NOT NULL CHECK (btrim(source_url) <> ''),
    published_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_story_evidence_story_id ON story_evidence (story_id);

CREATE TABLE IF NOT EXISTS video_jobs (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE RESTRICT,
    daily_run_id TEXT NOT NULL REFERENCES daily_runs(id) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (status IN (
        'CREATED', 'SCRIPTED', 'MEDIA_READY', 'RENDERED', 'VALIDATED',
        'UPLOADED', 'SCHEDULED', 'PUBLISHED', 'FAILED', 'REJECTED', 'RETRY_PENDING'
    )),
    resume_state TEXT NOT NULL DEFAULT '',
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 3 CHECK (max_attempts >= 1 AND attempts <= max_attempts),
    idempotency_key TEXT NOT NULL UNIQUE CHECK (btrim(idempotency_key) <> ''),
    workflow_id TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    validation JSONB,
    publication_id TEXT NOT NULL DEFAULT '',
    version INTEGER NOT NULL DEFAULT 1 CHECK (version >= 1),
    validated_at TIMESTAMPTZ,
    uploaded_at TIMESTAMPTZ,
    scheduled_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_video_jobs_daily_run_id ON video_jobs (daily_run_id);
CREATE INDEX IF NOT EXISTS idx_video_jobs_status ON video_jobs (status);
CREATE INDEX IF NOT EXISTS idx_video_jobs_workflow_id ON video_jobs (workflow_id) WHERE workflow_id <> '';

CREATE TABLE IF NOT EXISTS scripts (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    video_job_id TEXT NOT NULL REFERENCES video_jobs(id) ON DELETE CASCADE,
    version INTEGER NOT NULL CHECK (version >= 1),
    language TEXT NOT NULL CHECK (language = 'ru'),
    text TEXT NOT NULL CHECK (btrim(text) <> ''),
    estimated_duration_ms BIGINT NOT NULL CHECK (estimated_duration_ms BETWEEN 15000 AND 30000),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (video_job_id, version)
);

CREATE TABLE IF NOT EXISTS media_assets (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    video_job_id TEXT NOT NULL REFERENCES video_jobs(id) ON DELETE RESTRICT,
    asset_type TEXT NOT NULL CHECK (asset_type IN ('VOICEOVER', 'BACKGROUND', 'CLIP', 'RENDERED_VIDEO', 'THUMBNAIL')),
    bucket TEXT NOT NULL CHECK (btrim(bucket) <> ''),
    object_key TEXT NOT NULL CHECK (btrim(object_key) <> ''),
    uri TEXT NOT NULL CHECK (btrim(uri) <> ''),
    content_type TEXT NOT NULL CHECK (btrim(content_type) <> ''),
    size_bytes BIGINT NOT NULL CHECK (size_bytes > 0),
    duration_ms BIGINT NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
    checksum TEXT NOT NULL CHECK (btrim(checksum) <> ''),
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_media_assets_video_job_id ON media_assets (video_job_id);

CREATE TABLE IF NOT EXISTS publications (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    video_job_id TEXT NOT NULL UNIQUE REFERENCES video_jobs(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL DEFAULT 'youtube' CHECK (btrim(provider) <> ''),
    external_video_id TEXT NOT NULL DEFAULT '',
    idempotency_key TEXT NOT NULL UNIQUE CHECK (btrim(idempotency_key) <> ''),
    status TEXT NOT NULL CHECK (status IN ('PENDING', 'SCHEDULED', 'PUBLISHED', 'FAILED')),
    scheduled_at TIMESTAMPTZ,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_publications_external_video_id ON publications (external_video_id) WHERE external_video_id <> '';

CREATE TABLE IF NOT EXISTS generation_costs (
    id TEXT PRIMARY KEY CHECK (btrim(id) <> ''),
    video_job_id TEXT NOT NULL REFERENCES video_jobs(id) ON DELETE RESTRICT,
    provider TEXT NOT NULL CHECK (btrim(provider) <> ''),
    operation TEXT NOT NULL CHECK (btrim(operation) <> ''),
    units BIGINT NOT NULL CHECK (units >= 1),
    unit_cost_micro_usd BIGINT NOT NULL CHECK (unit_cost_micro_usd >= 0),
    amount NUMERIC(18, 6) NOT NULL CHECK (amount >= 0),
    currency TEXT NOT NULL DEFAULT 'USD' CHECK (btrim(currency) <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_generation_costs_video_job_id ON generation_costs (video_job_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type TEXT NOT NULL CHECK (btrim(entity_type) <> ''),
    entity_id TEXT NOT NULL CHECK (btrim(entity_id) <> ''),
    action TEXT NOT NULL CHECK (btrim(action) <> ''),
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_audit_events_entity ON audit_events (entity_type, entity_id, created_at DESC);

CREATE TABLE IF NOT EXISTS youtube_upload_journal (
    idempotency_key TEXT PRIMARY KEY CHECK (btrim(idempotency_key) <> ''),
    state TEXT NOT NULL CHECK (state IN ('PENDING', 'IN_PROGRESS', 'COMPLETED', 'FAILED', 'UNKNOWN')),
    video_id TEXT NOT NULL DEFAULT '',
    upload_status TEXT NOT NULL DEFAULT '',
    session_uri TEXT NOT NULL DEFAULT '',
    total_bytes BIGINT NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
    sent_bytes BIGINT NOT NULL DEFAULT 0 CHECK (sent_bytes >= 0),
    file_sha256 TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    privacy TEXT NOT NULL DEFAULT '',
    video_job_id TEXT NOT NULL DEFAULT '',
    publication_id TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    uploaded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_youtube_upload_journal_video_id ON youtube_upload_journal (video_id) WHERE video_id <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS youtube_upload_journal;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS generation_costs;
DROP TABLE IF EXISTS publications;
DROP TABLE IF EXISTS media_assets;
DROP TABLE IF EXISTS scripts;
DROP TABLE IF EXISTS video_jobs;
DROP TABLE IF EXISTS story_evidence;
DROP TABLE IF EXISTS stories;
DROP TABLE IF EXISTS daily_runs;
DROP TABLE IF EXISTS articles;
DROP TABLE IF EXISTS sources;
-- +goose StatementEnd
