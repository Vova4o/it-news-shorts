# Тестирование News Collector

## Unit-тесты

Все внешние HTTP-запросы в unit-тестах изолированы через `httptest.Server` или custom `http.RoundTripper`:

```bash
go test -v -race ./internal/news/...
```

Покрываются:
- RSS 2.0 и Atom парсинг, пустые/некорректные XML, кодировка `windows-1251`, условный GET (`304 Not Modified`).
- Hacker News API: объединение `top`/`new`/`best`, фильтрация `deleted`/`dead`/`job`/`comment`, `null`/404, ретраи на `429`.
- Нормализация URL, текста, дат и часовых поясов.
- Дедупликация и фильтрация 24-часового окна `[Start, End)`, включая приоритет `PublishedAt` над `SourceSubmittedAt`.
- Безопасность: запрет `file://`, блокировка `localhost`/приватных IP (в том числе при редиректах), ограничение редиректов и размера ответа.

## Integration-тесты

Запускаются против реального PostgreSQL (Docker Compose):

```bash
make test-integration
```

Проверяются:
- Сохранение, дедупликация между источниками, конкурентный `UpsertArticle` и формирование `DailySnapshot` (`internal/postgres/news_integration_test.go`).
- Частичный сбой источника (`HTTP 503` на одном источнике при успешном сохранении другого) в `NewsIngestionWorkflow` (`internal/workflows/news_integration_test.go`).

## End-to-End проверка на реальных источниках

```bash
docker compose -f deployments/docker-compose.yml up -d --build worker
go run ./cmd/cli news collect
go run ./cmd/cli news list --last 24h
go run ./cmd/cli news snapshot
go run ./cmd/cli news stats --json
```
