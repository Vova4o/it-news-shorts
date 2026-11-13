# Дедупликация новостей

Дедупликация защищает систему от повторного сохранения статей при периодическом запуске коллектора каждые 30 минут, при конкурентной работе Temporal Activities и при пересечении материалов между источниками (например, Hacker News и TechCrunch).

## Уровни дедупликации

1. **Уровень 1 — Source ID + External ID**:
   - Уникальный частичный индекс PostgreSQL `idx_articles_source_external_id` по `(source_id, external_id) WHERE external_id <> ''`.
2. **Уровень 2 — Canonical URL**:
   - Уникальный частичный индекс `idx_articles_canonical_url` по `canonical_url WHERE canonical_url <> ''`.
   - При совпадении URL между разными источниками запись не дублируется, а в таблице `article_sources` фиксируются все источники, обнаружившие материал.
   - Метрики Hacker News (`popularity_score`, `comments_count`, `source_submitted_at`) и `published_at` из RSS объединяются через `COALESCE` / `GREATEST`.
3. **Уровень 3 — Normalized Title**:
   - `NormalizeTitleForDedup` и `TitleSimilarity` вычисляют нормализованный заголовок и оценку схожести (Jaccard), но разные статьи с похожими заголовками **не** объединяются автоматически на этапе сбора. Семантическая группировка событий выполняется на следующем этапе (TASK 005).

## Конкурентность и идемпотентность

`NewsRepository.UpsertArticle` выполняет транзакционный поиск (`FOR UPDATE`) + `INSERT ... ON CONFLICT DO NOTHING` с повторным чтением при гонке конкурентных транзакций, что исключает ошибки уникальности и дубликаты при параллельном сборе.
