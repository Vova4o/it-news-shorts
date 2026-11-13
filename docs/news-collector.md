# News Collector Engine

Автоматический сервис сбора IT-новостей из реальных источников (Hacker News API и RSS/Atom-ленты) с нормализацией, фильтрацией по 24-часовому окну, дедупликацией и сохранением в PostgreSQL через Temporal Workflows.

## Архитектура

1. **Continuous Ingestion (`NewsIngestionWorkflow`)**:
   - Запускается каждые 30 минут через Temporal Schedule `news-ingestion-schedule` (настраивается через `NEWS_INGESTION_INTERVAL`).
   - Параллельно с ограничением concurrency (`NEWS_HTTP_MAX_CONCURRENCY`) собирает новости из включённых источников.
   - Использует условный HTTP-кэш (`ETag` / `Last-Modified` → HTTP `304 Not Modified`).
   - Сохраняет статьи и историю сборов в PostgreSQL.
2. **Daily Snapshot (`DailyNewsSnapshotWorkflow`)**:
   - Запускается ежедневно в 09:00 `Europe/Moscow` (06:00 UTC) через Temporal Schedule `daily-news-snapshot-schedule`.
   - Формирует срез статей за полуоткрытый интервал `[window_start, window_end)` длительностью 24 часа на основе уже накопленных данных в PostgreSQL.
   - Если данные не покрывают полные 24 часа (например, сразу после первого запуска), снимок помечается как `PARTIAL_COVERAGE`, иначе `FULL_COVERAGE`.

## Конфигурация (ENV)

| Переменная | По умолчанию | Описание |
|---|---|---|
| `NEWS_INGESTION_INTERVAL` | `30m` | Интервал фонового сбора |
| `NEWS_SCHEDULES_ENABLED` | `true` | Включение/отключение Temporal Schedules |
| `NEWS_HTTP_TIMEOUT` | `15s` | Таймаут одного HTTP-запроса |
| `NEWS_HTTP_MAX_BYTES` | `5242880` | Максимальный размер ответа (5 MiB) |
| `NEWS_HTTP_MAX_CONCURRENCY` | `5` | Максимум одновременных HTTP-запросов |
| `NEWS_HN_BASE_URL` | `https://hacker-news.firebaseio.com/v0` | Базовый URL Hacker News API |
| `NEWS_HN_LIMIT` | `30` | Лимит элементов с каждого списка HN (`top`, `new`, `best`) |
| `NEWS_RSS_LIMIT` | `50` | Лимит элементов с одной RSS/Atom-ленты |
| `NEWS_EXTRA_RSS_SOURCES` | `""` | JSON-массив дополнительных RSS-источников |

## Ручной запуск и диагностика (CLI)

```bash
# Список источников и их состояние
go run ./cmd/cli news sources

# Ручной запуск сбора через Temporal
go run ./cmd/cli news collect
go run ./cmd/cli news collect --source techcrunch --limit 10 --dry-run

# Просмотр собранных новостей за 24 часа
go run ./cmd/cli news list --last 24h

# Создание ежедневного снимка на дату (Europe/Moscow, окончание в 09:00 MSK)
go run ./cmd/cli news snapshot --date 2026-11-13

# Статистика и покрытие окна (поддерживается --json)
go run ./cmd/cli news stats --json
```

## Диагностика и перезапуск сбоев

- Ошибка одного источника фиксируется в `source_fetch_state.last_error` и `source_fetch_history`, при этом остальные источники продолжают сохраняться (`status = PARTIAL_FAILURE`).
- Посмотреть историю и статусы Workflow можно в Temporal UI (`http://localhost:8082`).
- Повторный запуск `go run ./cmd/cli news collect --source <id>` безопасен и идемпотентен: дубликаты не создаются.
