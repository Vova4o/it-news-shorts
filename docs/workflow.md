# Workflow: от новости до публикации

## Расписание

Каждый день в 09:00 Europe/Moscow (`domain.MoscowSchedule()`), смещение
UTC+3 фиксировано с 2014 года, поэтому часовой пояс задан `time.FixedZone` и
не зависит от tzdata в образе.

`DailySchedule.NextRun` возвращает ближайший запуск строго после переданного
момента: в 08:59:59 MSK — сегодняшние 09:00, ровно в 09:00:00 — следующие.
`DailySchedule.WindowFor(runAt)` даёт окно `[runAt-24h, runAt)`.

## Шаги прогона

1. **Создание прогона.** `DailyRun` с окном новостей и `MaxStories ≤ 3`.
2. **Сбор.** `NewsCollector.Collect(ctx, window)`; статьи вне окна отбрасываются,
   дубликаты по URL и ID — через `domain.Registry`.
3. **Отбор.** `StoryRanker` даёт оценки, `FactChecker` подтверждает факты,
   `editor.SelectStories` берёт не более трёх сюжетов: сортировка по оценке,
   при равенстве — более ранняя новость, дубли заголовков удаляются.
4. **Сценарий.** `ScriptGenerator` создаёт `Script` на русском с оценкой
   15–30 секунд.
5. **Медиа.** `TTSProvider` и `VideoGenerationProvider` создают `MediaAsset`,
   файлы кладутся через `ObjectStorage`.
6. **Монтаж.** `VideoRenderer` собирает шорт; `VideoValidator` проверяет
   длительность, аудио и разрешение и возвращает `VideoValidation`.
7. **Публикация.** `VideoPublisher.Upload`, затем `Schedule` на нужное время.
8. **Учёт.** Каждый вызов провайдера пишет `GenerationCost`; `analytics.Ledger`
   останавливает прогон при превышении дневного или повидео бюджета.
9. **Отчёт.** `Notifier` отправляет `DailyReport` в Telegram.

## Состояния VideoJob на этом пути

```
CREATED      создан VideoJob (пункт 2–3)
SCRIPTED     сценарий сохранён (4)
MEDIA_READY  озвучка и визуал готовы (5)
RENDERED     файл смонтирован (6)
VALIDATED    техническая проверка пройдена (6)
UPLOADED     видео загружено на YouTube (7)
SCHEDULED    публикация запланирована (7)
PUBLISHED    видео опубликовано (7)
```

Ветки: `FAILED` — ошибка шага, `RETRY_PENDING` — ожидание повтора,
`REJECTED` — сюжет или видео отброшены (например, не прошли факт-чек).

## Идемпотентность

- Ключ задания: `run-YYYY-MM-DD/story-NN/v1`. Повторный прогон с тем же ключом
  не создаёт второй `VideoJob` (в памяти — `domain.Registry`, позже — уникальный
  индекс PostgreSQL).
- Ключ публикации: `run-YYYY-MM-DD/story-NN/publish`. Повторный вызов
  `Upload` с тем же ключом должен вернуть существующую публикацию, а не создать
  новую.
- `VideoJob` владеет не более чем одной `Publication` (`AttachPublication`
  возвращает `ErrAlreadyPublished`), а `PUBLISHED` — терминальное состояние.

## Восстановление после ошибок

- Ошибка шага: `Transition(FAILED)`, причина сохраняется в логах и отчёте.
- `Transition(RETRY_PENDING)` увеличивает счётчик попыток и запоминает
  состояние возврата (`ResumeState`). Вернуться можно только в него — пропуск
  шагов невозможен.
- После `MaxAttempts` (по умолчанию 3) переход в `RETRY_PENDING` запрещён:
  задание уходит в `REJECTED` и попадает в отчёт.
- Temporal на следующем этапе переигрывает историю: вход прогона
  детерминирован, побочные эффекты идут только через порты.

## Ограничения расходов

`analytics.Budget{MaxDaily, MaxPerVideo}` проверяется до вызова провайдера:
`Ledger.Record` возвращает `domain.ErrBudgetExceeded`, и шаг не выполняется.
Деньги хранятся в микро-долларах (`domain.MicroUSD`), без float.
