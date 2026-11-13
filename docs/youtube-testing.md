# Тестирование YouTube-интеграции

## 1. Автоматические unit- и integration-тесты (без интернета)

Все автоматические тесты используют локальные `httptest.Server` / транспортные адаптеры и временные директории. Реальные запросы в Google/YouTube при `go test ./...` не выполняются.

```bash
env GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go vet ./...
env GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -count=1 ./...
env GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off go test -race -count=1 ./...
```

Покрытые сценарии:
- **OAuth (`internal/youtube/oauth_test.go`)**: обмен authorization code + PKCE, обновление истёкшего access token с сохранением `refresh_token`, `invalid_grant`, `ErrStateMismatch`, ошибки и дублирующиеся запросы callback, отсутствие credentials, проверка `ExpectedChannelID` до записи токена, защита от симлинков в git-репозиторий, конкурентный `Token()` под `-race`.
- **Resumable Upload (`internal/youtube/resumable_test.go`)**: успешная чанковая загрузка с сохранением `UploadStatus`/`UploadedAt`, `401`/`403`, `429` с `Retry-After` (секунды и HTTP-date без урезания), `500`/`503`, обрыв TCP и восстановление по `308 Range`, валидация некорректного `Range`, идемпотентный повтор, отказ при несовпадении файла для того же ключа, отказ при `journal == nil`, конкурентные вызовы с одинаковым и разными ключами под `-race`, восстановление после сбоя (`200`/`308`/`503`/`404`), наличие `ctx.Deadline()` на каждом запросе.
- **Client / Status / Schedule (`internal/youtube/client_test.go`)**: планирование в будущем, время в прошлом, запрет для `public` и `unlisted`, повторное назначение, сохранение изменяемых полей `status`, `dry-run` без `PUT`, `WaitProcessing` (`processing` -> `succeeded`, `failed`, `terminated`, `rejected`, `upload failed`, таймаут), связка `Adapter.Upload -> Adapter.Schedule` с проверкой `domain.Publication.Validate()`.
- **CLI (`cmd/cli/youtube_test.go`)**: автоматическое обновление истёкшего токена через локальный token endpoint, проверка `YOUTUBE_CHANNEL_ID`, стабильность `deriveIdempotencyKey` (независимость от пути и `mtime`), `dry-run` и запрет `--privacy public`.

## 2. Генерация тестового видеоролика

```bash
make test-video
```

Скрипт `scripts/make-test-video.sh` создаёт вертикальный MP4 (`1080x1920`, H.264 + AAC, 7 секунд) с надписью `IT NEWS API TEST`. Если ни один текстовый рендерер (`ffmpeg drawtext`, Python `PIL`, ImageMagick) недоступен, скрипт завершается с ошибкой вместо создания пустого видео без надписи.

## 3. Ручная live-проверка (после настройки OAuth)

```bash
# 1. Авторизация
go run ./cmd/cli youtube auth

# 2. Проверка канала
go run ./cmd/cli youtube channel

# 3. Dry-run и реальная private-загрузка
go run ./cmd/cli youtube upload --file ./testdata/test-short.mp4 --title "Integration Test"
go run ./cmd/cli youtube upload --file ./testdata/test-short.mp4 --title "Integration Test" --confirm-upload

# 4. Проверка статуса и ожидание обработки
go run ./cmd/cli youtube status --id VIDEO_ID --wait

# 5. Повторный вызов с тем же файлом и заголовком (дубль не создаётся)
go run ./cmd/cli youtube upload --file ./testdata/test-short.mp4 --title "Integration Test" --confirm-upload

# 6. Dry-run назначения времени публикации
go run ./cmd/cli youtube schedule --id VIDEO_ID --publish-at "2026-12-01 12:00"
```

## 4. Текущий статус локальной и live-проверки

- Существующий тестовый ролик `testdata/test-short.mp4` проверен (`H.264 1080x1920`, `AAC`, 7 секунд), наличие надписи `IT NEWS API TEST` подтверждено декодированием кадра.
- В текущем окружении `YOUTUBE_CLIENT_ID`, `YOUTUBE_CLIENT_SECRET` и `YOUTUBE_CHANNEL_ID` не заданы, а локальный файл токена отсутствует (пользователь подтвердил отсутствие OAuth credentials). Реальная OAuth-авторизация, live-загрузка в YouTube, получение настоящего `videoId` и live-проверка обработки/идемпотентности **не выполнялись** и остаются заблокированными до предоставления локальной OAuth-авторизации.
