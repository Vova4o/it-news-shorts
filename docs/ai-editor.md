# AI News Editor (TASK 005)

## Overview

The AI News Editor transforms a 24-hour Daily Snapshot of raw collected articles (`daily_snapshots` + `articles`) into up to three verified, deduplicated, ranked IT stories (`daily_selections` / `daily_selection_items`) stored in PostgreSQL.

## Pipeline Stages

1. **Load Daily Snapshot (`LoadDailySnapshot`)**: Loads the target date's `daily_snapshots` row, articles within the 24h window, and 7-day previously selected story fingerprints for repeat protection.
2. **Preprocess (`PreprocessArticles`)**: Removes empty titles, out-of-window articles, and duplicate canonical URLs; sorts by popularity/freshness and caps batch size.
3. **Semantic Clustering (`ClusterStories`)**: `editor.StoryClusterer` groups articles covering the same real-world IT event into `ClusteredStory` records without merging distinct product versions, vulnerabilities, or rumors.
4. **Claim Extraction (`ExtractClaims`)**: `editor.FactExtractor` extracts atomic factual claims anchored strictly to source `article_id`s and verbatim evidence quotes.
5. **Fact Verification (`VerifyClaims`)**: `editor.FactCheckerEngine` validates claims against source text, checks numeric/date grounding, detects contradictions, verifies independent sources (filtering syndicated reprints), and rejects any unverified story.
6. **Eligibility Gate (`FilterEligibleStories`)**: Separates `VerificationPassed=true` stories from rejected stories with explicit `RejectionReason`.
7. **Deterministic + AI Scoring (`ScoreStories`)**: `editor.StoryRankerEngine` obtains 0–100 sub-scores (`Impact`, `Interest`, `Freshness`, `Novelty`) and computes `FinalScore = Impact*0.35 + Interest*0.25 + Freshness*0.20 + Novelty*0.20` in deterministic Go code.
8. **TOP-3 Selection (`SelectTopStories`)**: `editor.DailyStorySelector` picks up to 3 verified, non-repeated stories with a max-2-per-category diversity rule when alternatives exist.
9. **Persistence & Report (`PersistEditorialResults`, `GenerateEditorialReport`)**: Atomically persists `editorial_runs`, `stories`, `story_articles`, `story_claims`, `claim_evidence`, `story_scores`, `daily_selections`, `daily_selection_items`, and `llm_usage`.

## CLI Commands

```bash
go run ./cmd/cli editor run --date 2026-11-13 [--dry-run] [--json]
go run ./cmd/cli editor top --date 2026-11-13 [--json]
go run ./cmd/cli editor explain --story-id <story-id> [--json]
go run ./cmd/cli editor stats --date 2026-11-13 [--json]
```
