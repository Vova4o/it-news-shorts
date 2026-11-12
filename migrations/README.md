# migrations

Каталог для SQL-миграций PostgreSQL.

На этапе TASK 001 база данных не подключена, поэтому миграций нет. Схема
появится вместе с репозиториями в следующем этапе и будет включать таблицы
`daily_runs`, `articles`, `stories`, `story_evidence`, `video_jobs`,
`scripts`, `media_assets`, `publications`, `generation_costs`.

Уникальные ограничения, которые обязана повторить схема:

- `video_jobs.idempotency_key` — уникальный ключ;
- `publications.video_job_id` — уникальный индекс (одна публикация на видео);
- `publications.idempotency_key` — уникальный ключ;
- `generation_costs.id` — первичный ключ.
