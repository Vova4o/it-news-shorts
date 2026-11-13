# Editor Testing Guide

## Unit & Race Tests

```bash
make ci
go test -v ./internal/editor -run TestQualityEvaluationDataset
```

## Integration Tests (PostgreSQL + Temporal TestSuite)

```bash
make test-integration
```

Covers:
- `TestEditorRepository_PersistenceAndIdempotency`: full schema persistence + idempotent re-run
- `TestDailyEditorialWorkflow_RealPostgresAndMockLLM`: Temporal workflow + real PG + transient retry recovery
- `TestEditorialBudgetExceededStopsSafely`: daily USD budget guard

## Dry-Run Against Real Snapshot

```bash
POSTGRES_DSN="postgres://it_news_app:it_news_local_dev@127.0.0.1:5433/it_news?sslmode=disable" \
  go run ./cmd/cli editor run --date 2026-11-13 --dry-run
```
