# Стратегия тестирования

## Принципы

- Тесты домена не требуют ни базы данных, ни сети, ни Docker: всё
  детерминировано, время передаётся явно.
- Основной стиль — table-driven tests с `t.Run` на каждый случай.
- Проверяются наблюдаемые контракты: ошибки через `errors.Is`, поля через
  `errors.As(*domain.ValidationError)`.
- Внешние сервисы не мокируются в production-коде. Когда появятся адаптеры,
  фейки будут жить только в тестовых файлах.

## Что покрыто сейчас

| Требование | Тест |
| --- | --- |
| Создание валидного `VideoJob` | `TestNewVideoJob`, `TestNewVideoJobDefaultsMaxAttempts` |
| Допустимые переходы | `TestTransitionTable` (успешные строки) |
| Запрещённые переходы | `TestTransitionTable` (строки с `ErrInvalidTransition`) |
| Публикация без валидации | `TestPublishWithoutValidationIsForbidden`, `TestFailedValidationBlocksUpload` |
| Повторный переход в то же состояние | `TestTransitionTable`, `TestSinglePublicationPerJob` |
| Обязательные поля | `TestNewVideoJobRejectsInvalidInput`, `TestModelValidation` |
| Длительность 15–30 секунд | `TestShortsDurationBounds`, `TestVideoValidationRules` |
| Время запуска и границы 24 часов | `TestMoscowScheduleNextRun`, `TestWindowForCoversPrevious24Hours`, `TestTimeWindowRejectsWrongLength` |
| Уникальность ID и ключей идемпотентности | `TestRegistryRejectsDuplicateIDs`, `TestRegistryRejectsDuplicateIdempotencyKeys`, `TestIdempotencyKeyFormat`, `TestIdentifierConstructors` |

Дополнительно: `TestRetryAttemptsAreLimited`, `TestRetryFromFailureKeepsResumeState`
(лимит попыток и возврат в `ResumeState`), `TestSelectStories*` (отбор сюжетов),
`TestLedger*` (бюджеты), `TestSampleArticlesAreValid` (фикстура
`testdata/articles.json` декодируется в модели и проходит валидацию),
`TestSourceValidation`.

## Запуск

```bash
go test ./...            # быстрый прогон
go test -race ./...      # гонки, обязательны в CI
go test -run TestTransitionTable -v ./internal/domain
go test -cover ./...
```

## CI

`.github/workflows/ci.yml` на каждый push и pull request:

1. `go build ./...`;
2. `go vet ./...`;
3. `go test ./...`;
4. `go test -race ./...`.

Форматирование проверяется локально: `make fmt` переформатирует файлы,
`make fmt-check` только сообщает о расхождениях. В CI шаг `gofmt` не включён,
чтобы пайплайн не зависел от версии форматтера.

Локальный аналог: `make ci`.

## Что появится дальше

- Контрактные тесты адаптеров (YouTube, LLM, TTS, MinIO) на записанных ответах.
- Тесты Temporal workflow через `testsuite.WorkflowTestSuite` с
  детерминированным временем.
- Интеграционные тесты PostgreSQL на `testcontainers-go` (отдельный тег
  `integration`, чтобы `go test ./...` оставался быстрым).
- Golden-тесты FFmpeg-рендера: длительность, разрешение, наличие аудио.
