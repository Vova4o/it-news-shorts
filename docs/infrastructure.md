# Инфраструктура (TASK 003)

## Состав Docker Compose

Файл конфигурации: [deployments/docker-compose.yml](../deployments/docker-compose.yml).
Production-оверрайд без dev-умолчаний: [deployments/docker-compose.prod.yml](../deployments/docker-compose.prod.yml).

Запускается 8 сервисов в изолированной bridge-сети `it_news_net`:

1. `postgres` (`postgres:16.16-alpine`) — PostgreSQL 16 с раздельными базами и ролями:
   - база приложения `it_news` (роль `it_news_app`);
   - базы Temporal `temporal` и `temporal_visibility` (роль `temporal`).
   Инициализация выполняется скриптом [deployments/postgres/init-dbs.sh](../deployments/postgres/init-dbs.sh).
2. `temporal-init` (`temporalio/admin-tools:1.32.2`) — инициализация и обновление схем `temporal` и `temporal_visibility` через `temporal-sql-tool --plugin postgres12`.
3. `temporal` (`temporalio/server:1.32.2`) — Temporal Server с PostgreSQL persistence (`SKIP_SCHEMA_SETUP=true`, без deprecated `temporalio/auto-setup`).
4. `temporal-ui` (`temporalio/ui:2.58.1`) — веб-интерфейс Temporal.
5. `minio` (`cgr.dev/chainguard/minio@sha256:96b6cf3e14084d3427ec46de10c2873af61b945ad9272eec266cfc8df551cba5`) — S3-совместимое объектное хранилище (pinned digest). Upstream MinIO Community Edition архивирован `2026-04-25`; клиент `internal/storage` использует стандартный AWS SDK for Go v2 (`UsePathStyle`) и совместим с любым S3-хранилищем.
6. `minio-init` (`cgr.dev/chainguard/minio-client@sha256:2e433473835685d7daf6a266dbbf44fc9d5479b24cb351b7d170492f4fe971d4`) — идемпотентная инициализация приватного бакета `it-news-media` (pinned digest).
7. `api` — Go HTTP API (`/healthz`, `/readyz`, `/metrics`), собирается из `golang:1.27.4-alpine3.24` (с сохранением минимальной директивы `go 1.22` в `go.mod`) на `alpine:3.24.4`.
8. `worker` — Go Temporal Worker (`IT_NEWS_QUEUE`, `/metrics`, `/healthz`).

## Локальные порты (только `127.0.0.1`)

На хосте стандартные порты `5432`, `8080` и `8081` заняты локальными сервисами, поэтому по умолчанию выбраны свободные loopback-порты (переопределяются через `.env`):

| Сервис | Переменная | Адрес по умолчанию |
| --- | --- | --- |
| PostgreSQL | `POSTGRES_PORT` | `127.0.0.1:5433` |
| Temporal gRPC | `TEMPORAL_PORT` | `127.0.0.1:7233` |
| Temporal UI | `TEMPORAL_UI_PORT` | `127.0.0.1:8082` |
| MinIO S3 API | `MINIO_API_PORT` | `127.0.0.1:9000` |
| MinIO Console | `MINIO_CONSOLE_PORT` | `127.0.0.1:9001` |
| Go API | `API_PORT` | `127.0.0.1:8083` |

Внешний доступ к PostgreSQL, Temporal и MinIO закрыт: публикация только на `127.0.0.1`.

## Основные команды

```bash
make infra-up        # сборка и запуск всех 8 сервисов
make infra-status    # статус контейнеров + проверка PG/MinIO/Temporal
make infra-logs      # просмотр логов
make migrate-up      # применение SQL-миграций приложения
make smoke-test      # запуск InfrastructureSmokeWorkflow + перепроверка БД и MinIO
make infra-down      # остановка контейнеров БЕЗ удаления persistent volumes
```

Опасная команда полного удаления данных: `make infra-destroy-volumes`.

## Мониторинг и логирование

- Логи `api` и `worker` выводятся в формате JSON (`timestamp`, `level`, `service`, `workflow_id`, `run_id`, `video_job_id`, `activity`, `duration_ms`, `error`) без секретов и паролей.
- Метрики Prometheus доступны на `GET /metrics` у `api` (`http://127.0.0.1:8083/metrics`) и у `worker` (`:9090/metrics` внутри сети):
  - `it_news_activity_duration_seconds`
  - `it_news_workflow_total`
  - `it_news_activity_retry_total`
  - `it_news_postgres_errors_total`
  - `it_news_minio_errors_total`
  - `it_news_dependency_duration_seconds`
- `GET /healthz` проверяет только живость процесса (`200 OK` даже при падении зависимостей).
- `GET /readyz` проверяет PostgreSQL, MinIO и Temporal и возвращает `503 Service Unavailable` с безопасной диагностикой при недоступности любой зависимости.
