# Стратегия тестирования

## Принципы

- Unit-тесты (`go test ./...`) не требуют Docker, PostgreSQL, MinIO, Temporal или доступа в Интернет.
- Интеграционные тесты изолированы тегом сборки `//go:build integration` и запускаются отдельно через `make test-integration`.
- Реальные загрузки на YouTube в обычных и интеграционных тестах не выполняются.

## Уровни тестов

| Уровень | Команда | Покрытие |
| --- | --- | --- |
| Unit | `make test-unit` | Домен, конфиг, observability, S3 key builder, Temporal `WorkflowTestSuite` (success, retries, permanent errors, timeout, cancel), YouTube client/journal |
| Race | `make test-race` | Детектор гонок для всех unit-тестов |
| Integration | `make test-integration` | Миграции Goose, CRUD/FK/UTC/optimistic locking, `PGJournal` без pool starvation + миграция файла, MinIO lifecycle/presign/failures, live `InfrastructureSmokeWorkflow` + idempotency, backup/restore в отдельную тестовую БД |
| Smoke | `make smoke-test` | End-to-end запуск `InfrastructureSmokeWorkflow` через CLI с независимой перепроверкой PostgreSQL и MinIO |

## CI (`.github/workflows/ci.yml`)

1. `gofmt`, `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `go test -race -count=1 ./...`.
2. Проверка отсутствия секретов в репозитории.
3. Валидация `docker-compose.yml` и `docker-compose.prod.yml` (`make compose-check`).
4. Поднятие изолированного Compose-стека, проверка `/readyz`, идемпотентности миграций, `make test-integration` и `make smoke-test`.
