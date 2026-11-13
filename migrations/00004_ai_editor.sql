-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS editorial_runs (
    id TEXT PRIMARY KEY,
    snapshot_id TEXT NOT NULL REFERENCES daily_snapshots(id) ON DELETE RESTRICT,
    run_date DATE NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED', 'DRY_RUN')),
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL DEFAULT 'v1',
    total_articles INT NOT NULL DEFAULT 0,
    total_stories INT NOT NULL DEFAULT 0,
    verified_stories INT NOT NULL DEFAULT 0,
    rejected_stories INT NOT NULL DEFAULT 0,
    selected_count INT NOT NULL DEFAULT 0,
    total_cost_usd NUMERIC(12,6) NOT NULL DEFAULT 0,
    error_message TEXT NOT NULL DEFAULT '',
    report_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_editorial_runs_date ON editorial_runs (run_date, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_editorial_runs_snapshot ON editorial_runs (snapshot_id);

ALTER TABLE stories
    ALTER COLUMN daily_run_id DROP NOT NULL;

ALTER TABLE stories
    ADD COLUMN IF NOT EXISTS editorial_run_id TEXT REFERENCES editorial_runs(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS snapshot_id TEXT REFERENCES daily_snapshots(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT 'OTHER_IT',
    ADD COLUMN IF NOT EXISTS cluster_confidence NUMERIC(5,4) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cluster_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS verification_status TEXT NOT NULL DEFAULT 'UNVERIFIED',
    ADD COLUMN IF NOT EXISTS rejection_reason TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS story_fingerprint TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_stories_editorial_run ON stories (editorial_run_id);
CREATE INDEX IF NOT EXISTS idx_stories_fingerprint ON stories (story_fingerprint);

CREATE TABLE IF NOT EXISTS story_articles (
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    article_id TEXT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('PRIMARY', 'CORROBORATING', 'UPDATE')),
    confidence NUMERIC(5,4) NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (story_id, article_id)
);

CREATE TABLE IF NOT EXISTS story_claims (
    id TEXT PRIMARY KEY,
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    claim_text TEXT NOT NULL,
    claim_type TEXT NOT NULL,
    entities_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    numbers_json JSONB NOT NULL DEFAULT '[]'::jsonb,
    verification_status TEXT NOT NULL CHECK (verification_status IN ('VERIFIED', 'PARTIALLY_VERIFIED', 'UNVERIFIED', 'CONTRADICTED', 'INSUFFICIENT_EVIDENCE')),
    verification_reason TEXT NOT NULL DEFAULT '',
    contradiction_note TEXT NOT NULL DEFAULT '',
    confidence NUMERIC(5,4) NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_story_claims_story_id ON story_claims (story_id);

CREATE TABLE IF NOT EXISTS claim_evidence (
    id TEXT PRIMARY KEY,
    claim_id TEXT NOT NULL REFERENCES story_claims(id) ON DELETE CASCADE,
    article_id TEXT NOT NULL REFERENCES articles(id) ON DELETE CASCADE,
    source_url TEXT NOT NULL,
    source_type TEXT NOT NULL DEFAULT '',
    quote TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    is_primary_source BOOLEAN NOT NULL DEFAULT FALSE,
    is_independent BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_claim_evidence_claim_id ON claim_evidence (claim_id);

CREATE TABLE IF NOT EXISTS story_scores (
    story_id TEXT PRIMARY KEY REFERENCES stories(id) ON DELETE CASCADE,
    editorial_run_id TEXT NOT NULL REFERENCES editorial_runs(id) ON DELETE CASCADE,
    impact_score NUMERIC(6,2) NOT NULL CHECK (impact_score >= 0 AND impact_score <= 100),
    interest_score NUMERIC(6,2) NOT NULL CHECK (interest_score >= 0 AND interest_score <= 100),
    freshness_score NUMERIC(6,2) NOT NULL CHECK (freshness_score >= 0 AND freshness_score <= 100),
    novelty_score NUMERIC(6,2) NOT NULL CHECK (novelty_score >= 0 AND novelty_score <= 100),
    final_score NUMERIC(6,2) NOT NULL CHECK (final_score >= 0 AND final_score <= 100),
    reasoning_summary TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    scored_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS daily_selections (
    id TEXT PRIMARY KEY,
    selection_date DATE NOT NULL UNIQUE,
    editorial_run_id TEXT NOT NULL REFERENCES editorial_runs(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL REFERENCES daily_snapshots(id) ON DELETE RESTRICT,
    selected_count INT NOT NULL DEFAULT 0,
    summary_notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS daily_selection_items (
    selection_id TEXT NOT NULL REFERENCES daily_selections(id) ON DELETE CASCADE,
    rank INT NOT NULL CHECK (rank >= 1 AND rank <= 3),
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    story_fingerprint TEXT NOT NULL,
    category TEXT NOT NULL,
    final_score NUMERIC(6,2) NOT NULL,
    selection_reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (selection_id, rank),
    UNIQUE (selection_id, story_id)
);

CREATE TABLE IF NOT EXISTS llm_usage (
    id TEXT PRIMARY KEY,
    editorial_run_id TEXT REFERENCES editorial_runs(id) ON DELETE SET NULL,
    operation TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    request_id TEXT NOT NULL DEFAULT '',
    input_tokens INT NOT NULL DEFAULT 0,
    output_tokens INT NOT NULL DEFAULT 0,
    cost_usd NUMERIC(12,6) NOT NULL DEFAULT 0,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL,
    error_message TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_llm_usage_created_at ON llm_usage (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_llm_usage_run_id ON llm_usage (editorial_run_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS llm_usage;
DROP TABLE IF EXISTS daily_selection_items;
DROP TABLE IF EXISTS daily_selections;
DROP TABLE IF EXISTS story_scores;
DROP TABLE IF EXISTS claim_evidence;
DROP TABLE IF EXISTS story_claims;
DROP TABLE IF EXISTS story_articles;

DROP INDEX IF EXISTS idx_stories_fingerprint;
DROP INDEX IF EXISTS idx_stories_editorial_run;

ALTER TABLE stories
    DROP COLUMN IF EXISTS story_fingerprint,
    DROP COLUMN IF EXISTS rejection_reason,
    DROP COLUMN IF EXISTS verification_status,
    DROP COLUMN IF EXISTS cluster_reason,
    DROP COLUMN IF EXISTS cluster_confidence,
    DROP COLUMN IF EXISTS category,
    DROP COLUMN IF EXISTS snapshot_id,
    DROP COLUMN IF EXISTS editorial_run_id;

DROP TABLE IF EXISTS editorial_runs;

-- +goose StatementEnd
