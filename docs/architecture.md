# Архитектура

## Принцип

Один Go-модуль `github.com/it-news-shorts/it-news-shorts`, слоистая структура
по мотивам Clean Architecture без лишних абстракций: домен не знает ни о базе
данных, ни о Temporal, ни об HTTP. Внешние сервисы описаны интерфейсами
(портами) в своих пакетах, реализации появятся на следующих этапах.

Правило зависимостей:

```
cmd/*  ->  internal/workflows  ->  internal/{news,ai,script,audio,video,youtube,storage,notify}
                                              \-> internal/domain
internal/{editor,analytics}  ->  internal/domain
internal/domain  ->  только стандартная библиотека
```

`internal/domain` импортирует только `time`, `strings`, `net/url`, `fmt`,
`errors`, `unicode`. Проверить это можно так:

```bash
go list -f '{{ join .Imports "\n" }}' ./internal/domain | grep 'it-news-shorts' || echo "домен независим"
```

## Слои

| Слой | Пакеты | Ответственность |
| --- | --- | --- |
| Домен | `internal/domain` | Модели, инварианты, машина состояний, расписание, идемпотентность |
| Прикладные правила | `internal/editor`, `internal/analytics` | Отбор сюжетов, бюджеты и расходы |
| Порты | `internal/news`, `ai`, `script`, `audio`, `video`, `youtube`, `storage`, `notify` | Контракты внешних сервисов |
| Оркестрация | `internal/workflows` | Контракт ежедневного прогона и набор шагов |
| Точки входа | `cmd/api`, `cmd/worker`, `cmd/cli` | Запуск, wiring (появится позже) |

## Компоненты

- **NewsCollector** — забирает IT-новости за `domain.TimeWindow` (ровно 24 часа).
- **StoryRanker** — LLM-ранжирование кандидатов, оценка 0..1.
- **FactChecker** — проверка фактов по независимым источникам.
- **ScriptGenerator** — русский сценарий с оценкой длительности.
- **TTSProvider** — озвучка, результат `MediaAsset` вида `VOICEOVER`.
- **VideoGenerationProvider** — визуальные клипы.
- **VideoRenderer** — FFmpeg-монтаж в 1080×1920, 15–30 секунд.
- **VideoValidator** — техническая проверка: длительность, аудиодорожка, разрешение.
- **VideoPublisher** — загрузка и планирование на YouTube.
- **ObjectStorage** — MinIO-совместимое хранилище.
- **Notifier** — отчёт в Telegram.
- **DailyRunWorkflow** — будущий Temporal workflow; вход детерминирован
  (`DailyRunInput`), побочные эффекты только через порты.

## Машина состояний

Единственная точка изменения состояния `VideoJob` — `Transition`. Она
проверяет статическую таблицу переходов и предусловия (валидация перед
загрузкой, наличие публикации перед публикацией, лимит попыток). Подробности —
[docs/domain.md](domain.md) и [ADR 0002](decisions/0002-video-state-machine.md).

## Что намеренно отсутствует

Реальные интеграции, база данных, Temporal, MinIO, FFmpeg, веб-интерфейс,
production-моки. Заглушки в `cmd/*` только печатают расписание и не содержат
секретов.
