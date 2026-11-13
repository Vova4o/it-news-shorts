# Резервное копирование и восстановление

## Важные правила

- **Никогда не восстанавливайте резервную копию поверх рабочей БД без явного подтверждения.** Для проверки всегда используйте отдельную тестовую БД.
- **Temporal** хранит собственные данные и историю выполнения в отдельных базах `temporal` и `temporal_visibility`. Для согласованного восстановления всей системы резервное копирование PostgreSQL должно охватывать все три базы (`it_news`, `temporal`, `temporal_visibility`) в рамках единого окна или snapshot.

## PostgreSQL: Backup

```bash
# Бэкап базы приложения
docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  pg_dump -U postgres --clean --if-exists it_news > backup-it_news.sql

# Бэкап баз Temporal (обязательно для согласованного восстановления)
docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  pg_dump -U postgres --clean --if-exists temporal > backup-temporal.sql

docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  pg_dump -U postgres --clean --if-exists temporal_visibility > backup-temporal-visibility.sql
```

## PostgreSQL: Restore в отдельную тестовую БД

```bash
docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  psql -U postgres -d postgres -c "CREATE DATABASE it_news_restore_test OWNER it_news_app;"

docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  psql -U postgres -v ON_ERROR_STOP=1 -d it_news_restore_test < backup-it_news.sql

# Проверка данных
docker compose -p it-news-shorts -f deployments/docker-compose.yml exec -T postgres \
  psql -U it_news_app -d it_news_restore_test -c "SELECT count(*) FROM daily_runs;"
```

Автоматический тест восстановления в отдельную тестовую БД: `TestBackupAndRestoreToSeparateTestDB` в [internal/postgres/postgres_integration_test.go](../internal/postgres/postgres_integration_test.go).

## MinIO: Backup и Restore

Для локального или S3-зеркалирования используйте `mc mirror`:

```bash
mc alias set itnews http://127.0.0.1:9000 "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
mc mirror --preserve itnews/it-news-media ./backups/minio-media/
```

Восстановление в бакет:

```bash
mc mirror --preserve ./backups/minio-media/ itnews/it-news-media
```

## Восстановление конфигурации

- Шаблон переменных окружения хранится в [.env.example](../.env.example).
- Секреты (`.env`, OAuth-токены, пароли БД, ключи MinIO) не коммитятся в Git и восстанавливаются из защищённого хранилища секретов.
- После восстановления БД и MinIO:
  1. Запустите `make infra-up`.
  2. Проверьте статус: `make infra-status` и `GET /readyz`.
  3. Проверьте целостность: `make smoke-test`.
