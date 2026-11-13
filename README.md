# it-news-shorts

Автономный сервис производства и публикации IT-новостей в YouTube Shorts.

## Текущий этап (TASK 003)

Реализован инфраструктурный слой, персистентность и движок рабочих процессов поверх доменной архитектуры (TASK 001) и YouTube-интеграции (TASK 002):

- **Docker Compose (8 сервисов):** PostgreSQL 16, `temporal-init`, Temporal Server (`temporalio/server`, без `auto-setup`), Temporal UI, MinIO, `minio-init`, Go API, Go Worker.
- **PostgreSQL 16 + Goose:** 12 таблиц (включая `youtube_upload_journal`), `pgx/v5` + `pgxpool`, raw SQL репозитории (`DailyRun`, `VideoJob`, `Publication`, `MediaAsset`, `Story`), транзакционные переходы состояния и optimistic concurrency.
- **MinIO (AWS SDK v2):** потоковая загрузка с проверкой размера и SHA-256, приватный бакет, presigned URLs, сверка сиротских объектов.
- **Temporal:** `InfrastructureSmokeWorkflow` и идемпотентные Activities с retry policy и детерминизмом.
- **API & CLI:** `GET /healthz`, `GET /readyz`, `GET /metrics` (Prometheus), `cli infra {smoke-test,status,check-db,check-storage,migrate-up,migrate-down,migrate-youtube-journal}`.

## Быстрый старт

```bash
cp .env.example .env
make infra-up
make infra-status
make smoke-test
```

Локальные адреса (loopback `127.0.0.1`):
- Go API: `http://127.0.0.1:8083` (`/healthz`, `/readyz`, `/metrics`)
- Temporal UI: `http://127.0.0.1:8082`
- MinIO Console: `http://127.0.0.1:9001`
- PostgreSQL: `127.0.0.1:5433` (`it_news`)

## Проверки

```bash
make ci                # fmt-check + vet + test-unit + test-race
make compose-check     # проверка dev и prod Compose-конфигураций
make test-integration  # интеграционные тесты (PG, MinIO, Temporal, backup/restore)
make smoke-test        # live InfrastructureSmokeWorkflow через CLI
```

## Документация

- [docs/architecture.md](docs/architecture.md) — архитектура и слои
- [docs/domain.md](docs/domain.md) — доменные модели и машина состояний
- [docs/infrastructure.md](docs/infrastructure.md) — Docker Compose, порты, логи и метрики
- [docs/database.md](docs/database.md) — PostgreSQL, миграции, репозитории и PGJournal
- [docs/storage.md](docs/storage.md) — MinIO / S3
- [docs/temporal.md](docs/temporal.md) — Temporal Server, Worker и smoke workflow
- [docs/testing.md](docs/testing.md) — стратегия тестирования и CI
- [docs/backup-restore.md](docs/backup-restore.md) — резервное копирование и восстановление
- [docs/deployment.md](docs/deployment.md) — локальное и production-развёртывание
- [docs/decisions](docs/decisions) — ADR (включая [0004-database-migrations.md](docs/decisions/0004-database-migrations.md))
