# Настройка Google OAuth 2.0 для YouTube

## 1. Создание проекта в Google Cloud Console

1. Откройте [Google Cloud Console](https://console.cloud.google.com/) и создайте проект `it-news-shorts`.
2. В разделе **APIs & Services -> Library** включите **YouTube Data API v3**.

## 2. Настройка OAuth Consent Screen и Client ID

1. Откройте **Google Auth Platform / OAuth consent screen**:
   - Выберите тип приложения (**External** для тестового Google-аккаунта или **Internal** для Google Workspace).
   - Укажите название приложения, email поддержки и разработчика.
   - Добавьте свой Google-аккаунт в список **Test users**, пока приложение находится в статусе **Testing**.
   - Добавьте scope `https://www.googleapis.com/auth/youtube.force-ssl`.
2. В разделе **Clients** создайте **OAuth Client ID** с типом **Desktop application**.
3. Сохраните `Client ID` и `Client Secret` в локальные переменные окружения (никогда не коммитьте их в git и не передавайте в чат):

```bash
export YOUTUBE_CLIENT_ID="..."
export YOUTUBE_CLIENT_SECRET="..."
export YOUTUBE_CHANNEL_ID="UC..." # опционально, но рекомендуется
```

## 3. Объём разрешений (`youtube.force-ssl`)

Интеграция запрашивает scope:

- `https://www.googleapis.com/auth/youtube.force-ssl` — широкий OAuth scope, предоставляющий полный доступ на чтение, изменение и удаление данных YouTube авторизованного аккаунта (включая загрузку видео, изменение метаданных/расписания, управление плейлистами/комментариями и удаление видео).
- Он используется потому, что YouTube Data API v3 не предоставляет более узкого scope, который одновременно разрешал бы `videos.insert`, `videos.list` и `videos.update`.
- Храните локальный токен строго вне репозитория с правами `0600` и используйте отдельный тестовый канал/аккаунт на этапе разработки.

## 4. Первичная авторизация

```bash
go run ./cmd/cli youtube auth
```

Что происходит:
- Клиент поднимает локальный loopback callback-сервер на `127.0.0.1:<ephemeral-port>/callback` и использует Authorization Code Flow с PKCE (`access_type=offline`, `prompt=consent`).
- После подтверждения в браузере код обменивается на токены.
- Если задан `YOUTUBE_CHANNEL_ID` (или флаг `--channel-id`), канал проверяется **до** записи токена на диск; при несовпадении существующие сохранённые учётные данные не перезаписываются.
- Токен сохраняется вне git-репозитория (`os.UserConfigDir()/it-news-shorts/youtube-token.json`, либо путь из `YOUTUBE_TOKEN_FILE`) с правами `0600`. Попытка указать путь внутри git-репозитория (включая симлинки) отклоняется.

## 5. Смена канала

1. Укажите новый `YOUTUBE_CHANNEL_ID` (или передайте `--channel-id`).
2. Запустите `go run ./cmd/cli youtube auth` и выберите нужный канал в браузере.
3. Проверьте активный канал командой `go run ./cmd/cli youtube channel`.

## 6. Отзыв доступа и повторная авторизация

- Чтобы отозвать доступ, откройте настройки Google-аккаунта (**Security -> Third-party apps & services**) и удалите доступ для `it-news-shorts`.
- При следующем запросе Google вернёт `invalid_grant` (в коде маппится в `youtube.ErrRefreshTokenInvalid`).
- После отзыва или истечения refresh token удалите локальный token file (при необходимости) и повторно выполните `go run ./cmd/cli youtube auth`.

## 7. Ограничения OAuth

- Приложения в статусе **Testing** в Google Auth Platform обычно получают refresh token со сроком жизни 7 дней; для постоянной автономной работы потребуется перевод приложения в **In production** и прохождение применимой верификации Google.
- Если Google не вернул `refresh_token` (например, при повторной выдаче без `prompt=consent`), отзовите доступ приложения в аккаунте Google и повторите `youtube auth`.
