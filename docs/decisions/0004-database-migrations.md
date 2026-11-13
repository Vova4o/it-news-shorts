# ADR 0004: Инструмент миграций PostgreSQL (Goose)

- Статус: принято
- Дата: 2026-11-13
- Этап: TASK 003

## Контекст

На этапе TASK 003 необходимо управлять схемой PostgreSQL 16 (`sources`,
`articles`, `stories`, `story_evidence`, `daily_runs`, `video_jobs`,
`scripts`, `media_assets`, `publications`, `generation_costs`, `audit_events`,
`youtube_upload_journal`) из Go-приложения (`cmd/cli`) и интеграционных тестов
без внешних бинарных утилит и без ORM.

## Решение

Выбран `github.com/pressly/goose/v3` в паре с `embed.FS` (`migrations/embed.go`)
и драйвером `github.com/jackc/pgx/v5/stdlib`.

Причины выбора:

1. Нативная поддержка Go `embed.FS` (`goose.SetBaseFS(migrations.FS)`), что
   позволяет запускать `migrate-up` и `migrate-down` прямо из `cmd/cli` и
   интеграционных тестов.
2. Чистые SQL-файлы (`-- +goose Up` / `-- +goose Down`) с транзакционным
   выполнением и без генерации кода/ORM.
3. Минимальное дерево зависимостей поверх уже используемого `pgx/v5`, в
   отличие от `golang-migrate`, который тянет множество драйверов разных БД.
4. Идемпотентность: повторный вызов `goose.Up` на актуальной БД не выполняет
   повторных изменений и завершается без ошибки.

## Последствия

- Все миграции хранятся в `migrations/*.sql` и встраиваются в бинарники на
  этапе компиляции.
- Для `make migrate-up` / `make migrate-down` достаточно скомпилировать или
  запустить `cmd/cli infra migrate-up|migrate-down`.
- Базы данных Temporal (`temporal`, `temporal_visibility`) управляются отдельно
  официальным `temporal-sql-tool` в сервисе `temporal-init` и не смешиваются с
  миграциями приложения.
