# Editorial Workflow (Temporal)

`DailyEditorialWorkflow` (`internal/workflows/editorial.go`) orchestrates the AI editorial pipeline using `EditorialActivities` (`internal/workflows/editorial_activities.go`).

## Activities

1. `LoadDailySnapshot`
2. `PreprocessArticles`
3. `ClusterStories`
4. `ExtractClaims`
5. `VerifyClaims`
6. `FilterEligibleStories`
7. `ScoreStories`
8. `SelectTopStories`
9. `GenerateEditorialReport`
10. `PersistEditorialResults`

## Reliability & Idempotency

- Non-retryable error types (`CONFIG_ERROR`, `LLM_CONFIG_ERROR`, `PROMPT_CONFIG_ERROR`, `BUDGET_EXCEEDED`) stop immediately without wasting retries.
- Transient LLM/network errors retry up to 3 attempts with exponential backoff.
- `editorial_runs`, `stories`, `story_articles`, `story_claims`, `claim_evidence`, `story_scores`, `daily_selections`, and `daily_selection_items` are persisted transactionally and idempotently.
