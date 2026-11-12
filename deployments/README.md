# deployments

Каталог для Docker Compose и инфраструктурных описаний.

На этапе TASK 001 контейнеризация не выполняется: поднимать PostgreSQL,
Temporal и MinIO запрещено условиями задачи. Здесь появятся:

- `docker-compose.yml` — api, worker, postgres, temporal, minio;
- `Dockerfile` для Go-бинарников;
- healthcheck-и и ограничения ресурсов.
