# База данных PostgreSQL 16

## Подключение и пул

- Драйвер: `github.com/jackc/pgx/v5` + `pgxpool`.
- Используется только raw SQL без ORM.
- Все запросы ограничены `context.Context` таймаутами (`POSTGRES_QUERY_TIMEOUT`, по умолчанию `5s`).
- Все временные метки хранятся и возвращаются в `UTC` (`TIMESTAMPTZ`).
- Денежные суммы (`generation_costs`) хранятся как `BIGINT` (`unit_cost_micro_usd`) и `NUMERIC(18, 6)` (`amount`), без `float`.

Локальное подключение (по умолчанию):

```text
postgres://it_news_app:it_news_local_dev@127.0.0.1:5433/it_news?sslmode=disable
```

## Миграции

Выбран `pressly/goose/v3` ([ADR 0004](decisions/0004-database-migrations.md)). SQL-файлы встроены в бинарник через [migrations/embed.go](../migrations/embed.go).

```bash
make migrate-up
make migrate-down
```

## Таблицы схемы `public`

1. `sources` — источники новостей.
2. `articles` — собранные статьи (`FK -> sources`, `CHECK (fetched_at >= published_at)`).
3. `daily_runs` — ежедневные прогоны (`status`, `workflow_id`, `version`, `window_start/window_end`).
4. `stories` — сюжеты (`FK -> daily_runs`, `score NUMERIC(6,5)`, `position 0..3`).
5. `story_evidence` — доказательства фактов (`UUID PK`, `FK -> stories, articles`).
6. `video_jobs` — агрегат производства (`idempotency_key UNIQUE`, `version`, `validation JSONB`, `publication_id`).
7. `scripts` — версии сценариев (`UNIQUE (video_job_id, version)`).
8. `media_assets` — метаданные объектов MinIO (`UNIQUE (bucket, object_key)`, `size_bytes > 0`, `checksum`).
9. `publications` — публикации (`video_job_id UNIQUE`, `idempotency_key UNIQUE`).
10. `generation_costs` — расходы провайдеров (`NUMERIC(18,6)` + `micro_usd`).
11. `audit_events` — журнал аудита переходов и операций (`UUID PK`, `JSONB`).
12. `youtube_upload_journal` — PostgreSQL-журнал идемпотентности загрузок YouTube из TASK 002.

## Инварианты и блокировки

- Переходы состояния `VideoJob` выполняются исключительно через `VideoJobRepository.Transition` в транзакции: `SELECT ... FOR UPDATE`, гидратация `domain.RehydrateVideoJob`, вызов `job.Transition`, проверка `version` (optimistic concurrency) и запись `audit_events`.
- `PGJournal` (`internal/postgres/youtube_journal.go`) реализует `youtube.UploadJournal`:
  - Локальная блокировка по ключу + распределённый `pg_try_advisory_lock` на **отдельном** соединении вне `queryPool`, чтобы держатели блокировок никогда не вызывали pool starvation для `Get`/`Put`/`FindByVideoID`.
  - Миграция файлового журнала (`cli infra migrate-youtube-journal`) идемпотентна, fail-closed при конфликтах и сохраняет состояния `COMPLETED`, `UNKNOWN`, `IN_PROGRESS`, `PENDING`, `FAILED`. Состояние `UNKNOWN` никогда не запускает повторную загрузку автоматически.

## Безопасный cutover журнала YouTube (file -> postgres)

Параллельная работа независимых file-backed и PostgreSQL-backed загрузчиков недопустима — это нарушает гарантии идемпотентности и может привести к дублирующим загрузкам.

1. **Остановка загрузчиков:** остановить все процессы/воркеры, использующие файловый журнал YouTube.
2. **Резервная копия:** сохранить защищённую резервную копию файла журнала и выполнить backup PostgreSQL (`pg_dump` в отдельное защищённое хранилище).
3. **Импорт и повторная сверка:** выполнить `cli infra migrate-youtube-journal` и при необходимости повторить команду для подтверждения идемпотентности. При любом конфликте (разный `video_id`, статус, `idempotency_key` или SHA256) команда завершается fail-closed с ненулевым кодом возврата.
4. **Переключение и запуск:** только после успешного импорта без конфликтов переключить конфигурацию backend журнала на `postgres` и возобновить uploader.
