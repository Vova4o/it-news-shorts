# Temporal Server и Go Worker

## Конфигурация

- Образ сервера: `temporalio/server:1.32.2` с PostgreSQL persistence (`temporal`, `temporal_visibility`).
- Образ `temporalio/auto-setup` **не используется**.
- Namespace: `it-news` (переменная `TEMPORAL_NAMESPACE`, регистрируется идемпотентно).
- Task Queue: `IT_NEWS_QUEUE` (переменная `TEMPORAL_TASK_QUEUE`).
- Temporal Address: `127.0.0.1:7233` (локально) / `temporal:7233` (в Docker-сети).
- Temporal UI: `http://127.0.0.1:8082` (переменная `TEMPORAL_UI_PORT`).

## Правила детерминизма

Внутри `InfrastructureSmokeWorkflow` ([internal/workflows/smoke.go](../internal/workflows/smoke.go)) запрещены прямые HTTP/SQL/файловые вызовы, `time.Now()`, `rand` и генерация UUID. `run_id` передаётся снаружи, время берётся через `workflow.Now(ctx)`, а все побочные эффекты вынесены в Activities ([internal/workflows/activities.go](../internal/workflows/activities.go)).

## Retry Policies и идемпотентность

- Все Activities имеют `StartToCloseTimeout` (`30s`).
- Временные ошибки (сеть, БД, MinIO) повторяются с exponential backoff (`InitialInterval=500ms`, `BackoffCoefficient=2.0`, `MaximumAttempts=5`).
- Ошибки валидации и постоянные нарушения домена типизируются как `PermanentError` (`NonRetryableErrorTypes`) и не повторяются.
- Повторный запуск с тем же business `run_id` не создаёт дубликатов `DailyRun`, `VideoJob` или `MediaAsset`.

## Smoke Test через CLI

```bash
make smoke-test
# или напрямую:
go run ./cmd/cli infra smoke-test
```

CLI запускает `InfrastructureSmokeWorkflow`, дожидается статуса `COMPLETED` и самостоятельно перепроверяет запись в PostgreSQL и SHA-256 объекта в MinIO.
