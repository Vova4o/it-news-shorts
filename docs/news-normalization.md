# Нормализация новостей и фильтрация по времени

## Единая модель `UnifiedArticle`

Все источники преобразуются в `news.UnifiedArticle` (`internal/news/types.go`):
- Обязательные поля: `ID`, `SourceID`, `Title`, `OriginalURL`, `CanonicalURL`, `SourceType`, `FetchedAt`, `DiscoveredAt`, `Language`.
- Явно опциональные поля (используют указатели, без подстановки фиктивных значений):
  - `PublishedAt *time.Time` (UTC)
  - `SourceSubmittedAt *time.Time` (UTC, для Hacker News)
  - `PopularityScore *int`
  - `CommentsCount *int`

## Нормализация URL (`news.NormalizeURL`)

1. Схема и хост приводятся к нижнему регистру.
2. Удаляются порты по умолчанию (`:80` для `http`, `:443` для `https`).
3. Удаляются URL-фрагменты (`#...`).
4. Удаляются известные tracking-параметры (`utm_*`, `fbclid`, `gclid`, `mc_cid`, `mc_eid`, `yclid`, `igshid`, `_hsenc`, `_hsmi`, `ref`, `spm`, `si`, `feature`).
5. Значимые query-параметры сохраняются и сортируются детерминированно.

## Нормализация текста (`news.NormalizeText`)

- Удаление блоков `<script>` / `<style>` и всех HTML-тегов.
- Декодирование HTML entities (`html.UnescapeString`).
- Удаление управляющих символов и схлопывание пробелов.
- Приведение к валидному UTF-8 и ограничение длины (500 символов для заголовка, 2000 для описания).
- Тексты статей трактуются как недоверенные данные и никогда не используются как инструкции.

## Даты и окно 24 часов (`Europe/Moscow`)

- Все даты приводятся к UTC (`ParseDate` поддерживает RFC3339, RFC1123/RFC822, варианты RSS и Unix timestamps).
- Отсутствующая дата остаётся `nil` и никогда не заменяется текущим временем.
- Для окна выпуска 09:00 `Europe/Moscow` используется полуоткрытый интервал `[window_start, window_end)`.
- `FilterByTimeWindow` фиксирует причину исключения (`missing_published_at`, `future_published_at`, `before_window`, `after_window`).
- Если у новости из Hacker News есть `PublishedAt`, для проверки свежести используется именно `PublishedAt`, а не `SourceSubmittedAt`.
