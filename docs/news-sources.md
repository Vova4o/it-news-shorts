# Источники новостей

## Встроенные источники

| ID | Название | Тип | URL |
|---|---|---|---|
| `hackernews` | Hacker News | `hackernews` | `https://hacker-news.firebaseio.com/v0` |
| `techcrunch` | TechCrunch | `rss` | `https://techcrunch.com/feed/` |
| `theverge` | The Verge | `atom` | `https://www.theverge.com/rss/index.xml` |
| `arstechnica` | Ars Technica | `rss` | `https://feeds.arstechnica.com/arstechnica/index` |

## Добавление нового источника

Через переменную окружения `NEWS_EXTRA_RSS_SOURCES`:

```bash
export NEWS_EXTRA_RSS_SOURCES='[
  {"id":"wired","name":"Wired","type":"rss","feed_url":"https://www.wired.com/feed/rss","language":"en","enabled":true,"limit":30}
]'
```

При старте воркера или при запуске `ListEnabledSources` источник автоматически регистрируется в таблице `sources`.

## Отключение источника

Обновите флаг `enabled` в PostgreSQL:

```sql
UPDATE sources SET enabled = FALSE, updated_at = NOW() WHERE id = 'theverge';
```

## Безопасность HTTP-клиента

- Разрешены только схемы `http` и `https`.
- Встроенная защита от SSRF: блокируются `localhost`, `.local`, `.internal`, loopback, private, link-local и multicast IP (в том числе после DNS-резолва и HTTP-редиректов).
- Ограничение редиректов (максимум 5) и размера ответа (`NEWS_HTTP_MAX_BYTES`).
- Ретраи с exponential backoff + jitter для сетевых таймаутов, `429` (с учётом `Retry-After`) и `5xx`.
