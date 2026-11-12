# it-news-shorts

Полностью автоматизированный сервис производства и публикации IT-новостей в
YouTube Shorts: сбор новостей за 24 часа, отбор до трёх сюжетов, проверка
фактов, сценарий на русском, озвучка, визуал, монтаж 15–30 секунд, публикация
по расписанию, учёт расходов и отчёт в Telegram.

## Текущий этап (TASK 001)

Реализован только фундамент: доменные модели, машина состояний, порты внешних
сервисов, планировщик 09:00 Europe/Moscow, документация и unit-тесты.

Осознанно **не** подключены: LLM API, TTS API, Video Generation API, YouTube
Data API, Temporal, PostgreSQL, MinIO, FFmpeg. Моков в production-коде нет:
есть только интерфейсы и доменная логика.

## Требования

- Go 1.22 или новее
- GNU Make (опционально, все команды доступны напрямую через `go`)

## Проверки

```bash
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

То же через Makefile:

```bash
make fmt      # gofmt -l -w .
make vet      # go vet ./...
make test     # go test ./...
make race     # go test -race ./...
make ci       # fmt + vet + test + race
make build    # сборка трёх бинарников в bin/
```

`make fmt` переформатирует файлы; чтобы проверить форматирование без правок,
используйте `make fmt-check`.

## Структура

```
cmd/api        HTTP API (пока заглушка, веб-интерфейс запрещён на этом этапе)
cmd/worker     будущий Temporal worker и ежедневный пайплайн
cmd/cli        операторские команды (schedule, version)
internal/domain     модели, идентификаторы, машина состояний, расписание
internal/news       порт NewsCollector
internal/editor     отбор сюжетов (не более трёх, без дублей)
internal/ai         порты StoryRanker и FactChecker
internal/script     порт ScriptGenerator
internal/audio      порт TTSProvider
internal/video      порты VideoGenerationProvider, VideoRenderer, VideoValidator
internal/youtube    порт VideoPublisher
internal/storage    порт ObjectStorage
internal/analytics  бюджет и журнал расходов
internal/notify     порт Notifier и отчёт о прогоне
internal/workflows  контракт DailyRunWorkflow и набор шагов
migrations          SQL-миграции (появятся вместе с PostgreSQL)
configs             конфигурация окружений (появятся вместе с загрузчиком)
deployments         Docker Compose и инфраструктура (следующие этапы)
testdata            фикстуры для тестов
docs                архитектура, workflow, домен, тестирование, roadmap, ADR
```

## Быстрый старт

```bash
make test
go run ./cmd/cli schedule
go run ./cmd/worker
```

`cmd/cli schedule` печатает ближайшие запуски и 24-часовые окна новостей.

## Документация

- [docs/architecture.md](docs/architecture.md) — компоненты и зависимости
- [docs/workflow.md](docs/workflow.md) — путь от новости до публикации
- [docs/domain.md](docs/domain.md) — модели и переходы состояний
- [docs/testing.md](docs/testing.md) — стратегия тестирования
- [docs/roadmap.md](docs/roadmap.md) — дальнейшие этапы
- [docs/decisions](docs/decisions) — архитектурные решения (ADR)
