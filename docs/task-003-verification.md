# TASK 003 — результат независимой проверки

Дата: 2026-11-13. Реализация: Antigravity, Hydrogen Medium (`gemini-4-hydrogen-medium`), четыре прохода с исправлениями по приёмке. Codex проверил код, воспроизвёл дефекты отдельными регрессионными тестами, повторно проверил финальное дерево и подготовил этот отчёт.

Рабочий инфраструктурный контур проверен. HTTP и API Temporal UI подтверждают настоящий Completed workflow и сохранённую историю. Визуальная проверка не выполнена: инструмент CUA вернул `browser access host unavailable`. Эта ошибка относится к инструменту проверяющего; она не подтверждает отсутствие подключённого браузера у пользователя и не означает неисправность Temporal UI. Просьба к пользователю подключить браузер была необоснованной.

## Docker Compose и состояние

Проект Compose: `it-news-shorts`, файл `deployments/docker-compose.yml`. Сеть `it_news_net`; persistent volumes PostgreSQL и MinIO сохраняются при `infra-down`. Чужие контейнеры не изменялись.

| Сервис | Образ / сборка | Итоговое состояние | Локальный порт |
| --- | --- | --- | --- |
| postgres | postgres:16.16-alpine | running, healthy | 127.0.0.1:5433 |
| temporal-init | temporalio/admin-tools:1.32.2 | exited, code 0 | — |
| temporal | temporalio/server:1.32.2 | running, healthy | 127.0.0.1:7233 |
| temporal-ui | temporalio/ui:2.58.1 | running, healthy | 127.0.0.1:8082 |
| minio | Chainguard, digest в Compose | running, healthy | 127.0.0.1:9000 / 9001 |
| minio-init | Chainguard client, digest в Compose | exited, code 0 | — |
| api | Dockerfile target api | running, healthy | 127.0.0.1:8083 |
| worker | Dockerfile target worker | running, healthy | metrics :9090 внутри сети |

Для обоих работающих бинарников независимо проверены `go1.27.4` и Alpine `3.24.4`. Локальные Go-проверки выполнялись существующим `go1.25.6`, без установки/обновления host toolchain. CI настроен на Go `1.27.4`.

`GET /readyz`: HTTP 200, PostgreSQL / MinIO / Temporal — `ok`. У Worker реально опубликованы счётчики выполненных и ошибочных workflow; при последней проверке `completed=10`, `failed=7` (счётчики процесса, сбрасываются при его перезапуске).

## PostgreSQL

Базы приложения и Temporal разделены: `it_news` / `temporal` / `temporal_visibility`. Владельцы: `it_news_app` / `temporal` / `temporal`; административная роль — `postgres`.

Таблицы приложения:

`articles`, `audit_events`, `daily_runs`, `generation_costs`, `media_assets`, `publications`, `scripts`, `sources`, `stories`, `story_evidence`, `video_jobs`, `youtube_journal_locks`, `youtube_upload_journal`.

Служебная таблица миграций: `goose_db_version`. Применены миграции `00001_init_schema.sql` и аддитивная `00002_hardening.sql`. Используются pgxpool, raw SQL и instance-based Goose Provider. Проверены CRUD, FK, UTC, транзакции, конкурирующие переходы VideoJob, fencing потерянной блокировки журнала, повторный импорт журнала и backup/restore в отдельную БД.

Тест bootstrap теперь работает в отдельном краткоживущем контейнере PostgreSQL, дважды запускает инициализацию и проверяет отсутствие синтетических паролей в stdout/stderr. После итогового прогона в основном сервере нет `init_db_*`, `init_role_*`, `init_trole_*`; временных контейнеров `it-news-init-test-*` нет. Ранее оставленные этим тестом пять фикстур и их grants удалены; реальные базы/роли/volumes сохранены.

## Проверки финального дерева

| Проверка | Результат |
| --- | --- |
| `make -s ci`: fmt-check, vet, unit, race | exit 0 |
| `make -s compose-check`: local + production override | exit 0 |
| `make -s test-integration` | exit 0, 23 теста, SKIP нет |
| `make -s smoke-test` | exit 0, реальный Temporal + PostgreSQL + MinIO |
| Дополнительные S3-регрессии проверяющего | 4/4 PASS |
| Дополнительные PG journal-регрессии | 2/2 PASS |
| Дополнительные domain restoration-регрессии | 2/2 PASS |
| Дополнительные DSN redaction-регрессии | 2/2 PASS |
| Проверка старого PG run и байтов MinIO после полного рестарта | PASS |
| Secret scanner из CI, включая его self-test | exit 0 |
| `git diff --check` | exit 0 |

Набор `integration` включает 9 PostgreSQL-тестов, 6 storage-тестов (живой lifecycle и регрессии) и 8 workflow-тестов (2 живых и 6 WorkflowTestSuite). Временные тестовые оверлеи проверяющего находятся вне репозитория; регрессии закреплены реализацией в постоянных тестах репозитория.

Критические исправления по результатам проверки: неизменяемое связывание business run ID с входом до S3-записи, условная загрузка S3 без перезаписи, сверка метаданных при повторе, единый контракт file/PG журнала, fencing владельца блокировки, сохранение FAILED при ошибке, восстановление доменных инвариантов, bounded I/O, защита ключей/TTL/отмены, safe SQL quoting, исключение секретов из логов и Docker-контекста.

## Результат реального InfrastructureSmokeWorkflow

```json
{
  "artifact_key": "smoke-tests/smoke-191c149d-00ac-4770-9231-b96f5353cd5c/result.txt",
  "business_run_id": "smoke-191c149d-00ac-4770-9231-b96f5353cd5c",
  "checksum": "1bb8fe95e63b397614337c3bb767a7a87c70cd5cc76367e4f3f72f45e3ec3a03",
  "checksum_verified": true,
  "database_persisted": true,
  "size_bytes": 44,
  "status": "COMPLETED",
  "temporal_run_id": "01a1d48a-017c-72d6-b5a2-c417d8596d79",
  "workflow_id": "smoke-smoke-191c149d-00ac-4770-9231-b96f5353cd5c"
}
```

CLI после завершения независимо прочитал DailyRun и MediaAsset из PostgreSQL, скачал объект MinIO и сверил размер 44 байта и SHA-256 `1bb8fe95e63b397614337c3bb767a7a87c70cd5cc76367e4f3f72f45e3ec3a03`. API Temporal UI вернул `WORKFLOW_EXECUTION_STATUS_COMPLETED` именно для указанного Run ID.

## Идемпотентность

Business run ID: `parent-final-0db269b02c964445b1635bb7979e9d63`.

Четыре последовательных вызова: исходный payload → тот же payload → другой payload → исходный payload. Коды выхода: `0 / 0 / 1 / 0`; третий вызов завершился постоянным idempotency conflict до изменения объекта. Исходный SHA-256 `407b8606128340ebdf485bdfed1f0706772d8f59811365748774ca9bb3fde90e` сохранился.

Независимый SQL-запрос после проверки: ровно 1 DailyRun, 1 Story, 1 VideoJob, 1 MediaAsset; DailyRun остаётся `COMPLETED`. Конкурирующие запуски с разным payload дополнительно проверены живым интеграционным тестом.

## Восстановление

Antigravity выполнил отказы PostgreSQL, MinIO и Temporal: `/healthz` оставался 200, `/readyz` становился 503 и после восстановления возвращался в 200. Выполнен полный down/up собственного стека с сохранением volumes, а также остановка Worker во время уже начатой Activity.

Проверяющий отдельно подтвердил после этих действий:

- Старый run `smoke-e4ba8ff5-9577-4929-a6e6-11dd5da6571f` остаётся `COMPLETED`; старый объект `smoke-tests/smoke-e4ba8ff5-9577-4929-a6e6-11dd5da6571f/result.txt` читается, 44 байта, SHA-256 `1bb8fe95e63b397614337c3bb767a7a87c70cd5cc76367e4f3f72f45e3ec3a03` совпадает с БД.
- Старая история `smoke-smoke-e4ba8ff5-9577-4929-a6e6-11dd5da6571f`, Run ID `01a1d472-415f-77e6-9713-c2713b3bc699`: 29 событий, последнее — WorkflowExecutionCompleted.
- История `smoke-inflight-1794565628` показывает Activity attempt 2 после `StartToClose timeout` и смены Worker identity с `1@cb670e6ee19b@` на `1@563f79666a94@`; workflow завершён Completed. Это восстановление незавершённой Activity, а не только обработка очереди после старта Worker.

## Изменённые файлы

Реализация Antigravity:

- [.env.example](../.env.example)
- [.github/workflows/ci.yml](../.github/workflows/ci.yml)
- [Makefile](../Makefile)
- [README.md](../README.md)
- [cmd/api/main.go](../cmd/api/main.go)
- [cmd/cli/main.go](../cmd/cli/main.go)
- [cmd/worker/main.go](../cmd/worker/main.go)
- [docs/testing.md](../docs/testing.md)
- [go.mod](../go.mod)
- [go.sum](../go.sum)
- [internal/domain/models.go](../internal/domain/models.go)
- [internal/domain/videojob.go](../internal/domain/videojob.go)
- [internal/youtube/client.go](../internal/youtube/client.go)
- [internal/youtube/journal.go](../internal/youtube/journal.go)

## Созданные файлы

Реализация Antigravity:

- [.dockerignore](../.dockerignore)
- [cmd/cli/infra.go](../cmd/cli/infra.go)
- [deployments/Dockerfile](../deployments/Dockerfile)
- [deployments/docker-compose.prod.yml](../deployments/docker-compose.prod.yml)
- [deployments/docker-compose.yml](../deployments/docker-compose.yml)
- [deployments/postgres/init-dbs.sh](../deployments/postgres/init-dbs.sh)
- [deployments/temporal/dynamicconfig.yaml](../deployments/temporal/dynamicconfig.yaml)
- [docs/backup-restore.md](../docs/backup-restore.md)
- [docs/database.md](../docs/database.md)
- [docs/decisions/0004-database-migrations.md](../docs/decisions/0004-database-migrations.md)
- [docs/deployment.md](../docs/deployment.md)
- [docs/infrastructure.md](../docs/infrastructure.md)
- [docs/storage.md](../docs/storage.md)
- [docs/temporal.md](../docs/temporal.md)
- [internal/config/config.go](../internal/config/config.go)
- [internal/config/config_test.go](../internal/config/config_test.go)
- [internal/domain/rehydrate_test.go](../internal/domain/rehydrate_test.go)
- [internal/observability/logger.go](../internal/observability/logger.go)
- [internal/observability/metrics.go](../internal/observability/metrics.go)
- [internal/observability/observability_test.go](../internal/observability/observability_test.go)
- [internal/postgres/daily_run_repo.go](../internal/postgres/daily_run_repo.go)
- [internal/postgres/db.go](../internal/postgres/db.go)
- [internal/postgres/errors.go](../internal/postgres/errors.go)
- [internal/postgres/media_asset_repo.go](../internal/postgres/media_asset_repo.go)
- [internal/postgres/postgres_integration_test.go](../internal/postgres/postgres_integration_test.go)
- [internal/postgres/publication_repo.go](../internal/postgres/publication_repo.go)
- [internal/postgres/story_repo.go](../internal/postgres/story_repo.go)
- [internal/postgres/video_job_repo.go](../internal/postgres/video_job_repo.go)
- [internal/postgres/youtube_journal.go](../internal/postgres/youtube_journal.go)
- [internal/storage/s3.go](../internal/storage/s3.go)
- [internal/storage/s3_integration_test.go](../internal/storage/s3_integration_test.go)
- [internal/storage/s3_test.go](../internal/storage/s3_test.go)
- [internal/workflows/activities.go](../internal/workflows/activities.go)
- [internal/workflows/smoke.go](../internal/workflows/smoke.go)
- [internal/workflows/smoke_integration_test.go](../internal/workflows/smoke_integration_test.go)
- [internal/workflows/smoke_test.go](../internal/workflows/smoke_test.go)
- [internal/workflows/temporal_client.go](../internal/workflows/temporal_client.go)
- [migrations/00001_init_schema.sql](../migrations/00001_init_schema.sql)
- [migrations/00002_hardening.sql](../migrations/00002_hardening.sql)
- [migrations/embed.go](../migrations/embed.go)

Проверяющий создал этот отчёт: `docs/task-003-verification.md`.

## Ограничения проверки

- Визуальный просмотр [Temporal UI](http://127.0.0.1:8082) не выполнен из-за ошибки инструмента CUA `browser access host unavailable`. Проверены HTTP и API UI, включая статус Completed и сохранённую историю. Состояние браузера пользователя по этой ошибке не установлено; запрос действий по его настройке отозван. Визуальная проверка остаётся непроведённой.
- Live YouTube OAuth/upload не выполнялся: пользователь сообщил, что OAuth credentials пока нет. Unit-тесты прежней YouTube-интеграции прошли; реальные операции журнала пользователя не переносились.
- Удалённый GitHub Actions pipeline и production deployment не запускались. Добавлена конфигурация CI, проверки выполнены локально. TASK004 не начиналась.
- Создан локальный commit `feat: add PostgreSQL Temporal and MinIO infrastructure`. Push для TASK003 не выполнялся.
