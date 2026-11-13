# Объектное хранилище MinIO (S3)

## Архитектура

Реализация: [internal/storage/s3.go](../internal/storage/s3.go) на базе **AWS SDK for Go v2** (`service/s3`) с `UsePathStyle = true` для совместимости с MinIO и любым другим S3-провайдером.

`*storage.S3Store` реализует как расширенный S3-контракт (`PutObject`, `GetObject`, `StatObject`, `DeleteObject`, `ObjectExists`, `PresignGetObject`), так и доменный порт `storage.ObjectStorage`.

## Безопасность и целостность

- Бакет по умолчанию (`it-news-media`) создаётся приватным (`ACL: private`, анонимное чтение отключено в `minio-init`).
- Потоковая загрузка с ограничением максимального размера (`MINIO_MAX_OBJECT_BYTES`, по умолчанию 256 MiB) и вычислением `SHA-256` и точного `size_bytes`.
- Временные presigned URLs с ограниченным TTL (`MINIO_PRESIGN_TTL`, по умолчанию `15m`).
- Нормализация ключей через `BuildDailyObjectKey`:
  - `daily/YYYY-MM-DD/{video_job_id}/scripts/`
  - `daily/YYYY-MM-DD/{video_job_id}/images/`
  - `daily/YYYY-MM-DD/{video_job_id}/audio/`
  - `daily/YYYY-MM-DD/{video_job_id}/video/`
  - `daily/YYYY-MM-DD/{video_job_id}/final/`
- Метаданные `MediaAsset` записываются в PostgreSQL только после подтверждённой загрузки и проверки чтения/checksum.
- Для частичных отказов предусмотрена безопасная сверка осиротевших объектов `ReconcileOrphans` (с режимом `dryRun` и обязательным возрастным порогом `olderThan`).

## Доступ к MinIO Console

- S3 API: `http://127.0.0.1:9000`
- Web Console: `http://127.0.0.1:9001` (учётные данные из `.env`: `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY`).
