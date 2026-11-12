# Домен

## Модели

| Модель | Назначение | Ключевые правила |
| --- | --- | --- |
| `Article` | Новость из источника | http(s) URL, непустые title/source, `FetchedAt >= PublishedAt`, известный язык |
| `StoryEvidence` | Подтверждение факта | ссылка на статью, цитата, источник, дата |
| `Story` | Сюжет для шорта | оценка 0..1, позиция 0..3, минимум одно доказательство |
| `DailyRun` | Ежедневный прогон | окно ровно 24 часа, `MaxStories` 1..3, известное состояние |
| `Script` | Сценарий | русский язык, непустой текст, длительность 15–30 с, версия ≥ 1 |
| `MediaAsset` | Медиафайл | известный вид, допустимая схема URI, размер > 0, checksum, провайдер |
| `VideoJob` | Агрегат производства | идентификаторы, ключ идемпотентности, лимит попыток, машина состояний |
| `Publication` | Публикация на YouTube | уникальный `VideoJobID`, ключ идемпотентности, для `PUBLISHED` нужен YouTube ID |
| `GenerationCost` | Расход провайдера | `Units ≥ 1`, `Total == Units × UnitCost`, неотрицательные суммы |

Идентификаторы — разные типы (`ArticleID`, `StoryID`, `DailyRunID`,
`VideoJobID`, `ScriptID`, `MediaAssetID`, `PublicationID`, `CostID`), поэтому
их нельзя перепутать на этапе компиляции. Конструкторы (`domain.NewVideoJobID`
и т. п.) отклоняют пустые значения и пробелы.

## Состояния VideoJob

```
CREATED -> SCRIPTED -> MEDIA_READY -> RENDERED -> VALIDATED -> UPLOADED -> SCHEDULED -> PUBLISHED
```

Дополнительно: `FAILED`, `REJECTED`, `RETRY_PENDING`.

## Разрешённые переходы

| Из | В |
| --- | --- |
| `CREATED` | `SCRIPTED`, `FAILED`, `REJECTED`, `RETRY_PENDING` |
| `SCRIPTED` | `MEDIA_READY`, `FAILED`, `REJECTED`, `RETRY_PENDING` |
| `MEDIA_READY` | `RENDERED`, `FAILED`, `REJECTED`, `RETRY_PENDING` |
| `RENDERED` | `VALIDATED`, `FAILED`, `REJECTED`, `RETRY_PENDING` |
| `VALIDATED` | `UPLOADED`, `FAILED`, `REJECTED`, `RETRY_PENDING` |
| `UPLOADED` | `SCHEDULED`, `FAILED`, `RETRY_PENDING` |
| `SCHEDULED` | `PUBLISHED`, `FAILED`, `RETRY_PENDING` |
| `FAILED` | `RETRY_PENDING`, `REJECTED` |
| `RETRY_PENDING` | только `ResumeState` |
| `PUBLISHED` | — (терминальное) |
| `REJECTED` | — (терминальное) |

## Запреты и как они обеспечены

1. **Публикация без валидации.** Переход в `UPLOADED` требует
   `VideoValidation{Passed: true}`, а в `SCHEDULED`/`PUBLISHED` — ещё и
   привязанной публикации. Иначе `ErrValidationRequired` /
   `ErrValidationFailed` / `ErrPublicationNotAllowed`.
2. **Пропуск состояний.** `Transition` сверяется со статической таблицей, любой
   пропуск даёт `ErrInvalidTransition`.
3. **Повторная публикация одного `VideoJob`.** `AttachPublication` разрешён
   только в `UPLOADED`/`SCHEDULED` и только один раз (`ErrAlreadyPublished`);
   из `PUBLISHED` переходов нет.
4. **Повтор в то же состояние.** Возвращает `ErrSameState`.
5. **Бесконечные повторы.** `beginRetry` требует `Attempts < MaxAttempts`,
   иначе `ErrRetriesExhausted`.

## Валидация

`Validate()` есть у каждой модели. Ошибки типизированы:

- `*domain.ValidationError{Field, Err}` — указывает поле;
- `errors.Is(err, domain.ErrMissingField)` — обязательное поле;
- `errors.Is(err, domain.ErrInvalidValue)` — недопустимое значение;
- `errors.Is(err, domain.ErrDurationOutOfRange)` — длительность вне 15–30 с;
- `errors.Is(err, domain.ErrInvalidWindow)` — окно не равно 24 часам.

## Расписание

`domain.MoscowSchedule()` — 09:00 MSK. `NextRun` строго в будущем, `WindowFor`
даёт `[runAt-24h, runAt)`: новость, опубликованная ровно 24 часа назад,
включается; опубликованная ровно в момент запуска — нет.
