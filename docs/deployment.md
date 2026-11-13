# Развёртывание (Local & Production)

## Локальное развёртывание

1. Скопируйте `.env.example` в `.env` (или используйте dev-умолчания):
   ```bash
   cp .env.example .env
   ```
2. Запустите инфраструктуру:
   ```bash
   make infra-up
   ```
3. Проверьте готовность:
   ```bash
   make infra-status
   curl -fsS http://127.0.0.1:8083/readyz
   ```
4. Выполните smoke test:
   ```bash
   make smoke-test
   ```

## Production Override

Файл [deployments/docker-compose.prod.yml](../deployments/docker-compose.prod.yml) отключает все локальные dev-пароли и `AUTO_MIGRATE`, включает `POSTGRES_SSLMODE=require` и `MINIO_USE_SSL=true`, и требует явной передачи секретов и TLS-совместимых endpoints (`POSTGRES_HOST`, `MINIO_ENDPOINT`, `TEMPORAL_ADDRESS`, при необходимости `SSL_CERT_FILE`):

```bash
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.prod.yml up -d
```

Предварительные требования для production-запуска:
- Внешние или TLS-терминированные `PostgreSQL` (`POSTGRES_SSLMODE=require` / `verify-full`), `S3/MinIO` (`MINIO_USE_SSL=true`) и `Temporal`.
- Доверительный CA-бандл (`SSL_CERT_FILE`, по умолчанию `/etc/ssl/certs/ca-certificates.crt`) при использовании приватных сертификатов.
- Применение миграций отдельным контролируемым шагом (`cli infra migrate-up`) до выката новой версии `worker`/`api`.

## Восстановление после сбоя

- **Остановка без потери данных:** `make infra-down` сохраняет `postgres_data` и `minio_data`.
- **Падение Worker во время выполнения Workflow:** после повторного запуска `worker` Temporal Server переназначает незавершённые задачи из `IT_NEWS_QUEUE`, а идемпотентные Activities не создают дубликатов.
- **Просмотр логов:** `make infra-logs` (JSON-логи с `workflow_id`, `run_id`, `activity`, `duration_ms`, `error`).
