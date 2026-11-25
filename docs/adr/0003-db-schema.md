# ADR-0003. Схема БД: модель хранения Matrix в PostgreSQL

- Статус: предложено
- Дата: 2026-11-17
- Референс: `element-hq/synapse` @ `40f63e0`, `synapse/storage/schema/main/full_schemas/72/full.sql.postgres`
  и delta-миграции `synapse/storage/schema/main/delta/*`; правила проекта — ADR-0002.

## Контекст

Схема — фундамент для всех последующих задач (аутентификация, комнаты, `/sync`, позже федерация).
Требования к схеме: таблицы `users`, `devices`, `access_tokens`, `rooms`, `room_aliases`, `events`,
`room_memberships`, `room_state`, `transactions`; индексы `(room_id, stream_ordering)`,
уникальность `(room_id, state_key, type)`, `event_id UNIQUE`; миграции golang-migrate up/down;
слой доступа через pgx; интеграционный тест на вставку/выборку в stream-порядке.

Ограничение: портируем Synapse (ADR-0002), поэтому схема должна повторять **поведение** хранения,
но не внутренние механизмы Python-реализации.

## Решение

Схема — `migrations/0001_init.up.sql` (ядро: AUTH, ROOMS, EVENTS, SYNC) и `0002_media.up.sql`
(метаданные MEDIA, последний блок P0); откаты — парные `*.down.sql`, формат и
нумерация golang-migrate.
Доступ — `internal/store` (pgx + явные репозитории, без sqlc на этом этапе).

### Соответствие таблицам Synapse

| Наша таблица | Synapse | Отличие и причина |
|---|---|---|
| `users(user_id, localpart, password_hash, display_name, avatar_url, is_admin, deactivated, created_at)` | `users(name, password_hash, creation_ts, admin, …)` | `user_id` = `@localpart:server` как первичный ключ (в Synapse `name`), `localpart` вынесен отдельной колонкой с UNIQUE для проверки занятости (`/register/available`); `avatar_url` — сразу, чтобы не добавлять ALTER при реализации аутентификации |
| `devices(user_id, device_id, display_name, created_at, last_seen_ip, last_seen_at)` | `devices(user_id, device_id, display_name, last_seen, ip, user_agent)` | составной PK, `last_seen` разложен на время + IP; `user_agent` не храним (не нужен для поведения CS-API) |
| `access_tokens(id, token_hash, user_id, device_id, refresh_hash, created_at, expires_at, last_validated_at, revoked_at)` | `access_tokens(id, token, user_id, device_id, valid_until_ms, …)` | **храним только SHA-256 хеш** токена (Synapse — открытый текст): утечка дамп не даёт войти; `revoked_at` вместо `used` для аудита |
| `rooms(room_id, room_version, creator, is_public, created_at)` | `rooms(room_id, is_public, creator, room_version, has_auth_chain_index)` | без `has_auth_chain_index` (оптимизация федерации, вернём вместе с auth chain) |
| `room_aliases(room_alias, room_id, creator, created_at)` | `room_aliases(room_alias, room_id, creator)` | + `created_at` для сортировки; PK по алиасу |
| `events(event_id, room_id, type, sender, state_key, content, prev_events, auth_events, depth, origin_server_ts, origin, received_at, stream_ordering, rejected_reason)` | `events(topological_ordering, event_id, type, room_id, content, depth, origin_server_ts, received_ts, sender, stream_ordering, state_key, rejection_reason, outlier)` | `content` — **jsonb** вместо text (в Synapse JSON разбирается на каждом чтении); `prev_events`/`auth_events` — `text[]` вместо табличных связей `event_edges` (нужны для DAG `/messages`, `/context`); `topological_ordering` не дублируем — порядок в DAG берём из `depth`; `outlier` появится вместе с федерацией |
| `room_memberships(event_id, room_id, user_id, sender, membership, display_name, avatar_url, forgotten, created_at)` | `room_memberships(event_id, user_id, sender, room_id, membership, forgotten, display_name, avatar_url)` | почти 1:1; `user_id` **без FK** — федерация приносит удалённых пользователей |
| `room_state(room_id, type, state_key, event_id, updated_at)` | `current_state_events(room_id, type, state_key, event_id, membership)` + `state_groups_state` | у нас **прямая материализация** текущего состояния (без state groups): проще, а delta `/sync` будет строиться по `stream_ordering` (`current_state_delta_stream` - вместе с проектированием `/sync` в ADR-0005, если понадобится) |
| `transactions(user_id, device_id, endpoint, txn_id, room_id, event_id, inserted_at)` | `event_txn_id(room_id, user_id, token_id, txn_id, event_id, inserted_ts)` | ключ шире: идемпотентность не только `/send`, но и `/sendToDevice`, `/redact`; `device_id` вместо `token_id` (у нас токен = устройство) |
| `media(content_uri, media_id, server_name, uploader, content_type, size_bytes, sha256, file_name, storage_key, is_remote, blocked, created_at, last_accessed_at)` | `media_repository` + `local_media_repository`, `remote_media_cache` | в БД только метаданные: байты в файловом хранилище/S3 по `storage_key`; `sha256` для дедупликации вместо отдельных таблиц thumbs (миниатюры — P1, добавятся колонкой/таблицей без переделки) |
| — | `event_json`, `event_edges`, `state_groups*`, `event_auth`, `event_signatures`, `room_stats*` | не нужны до федерации: DAG-связи в массивах, подписи/статистика — вместе с федерацией и метриками |

### Ключевые решения

1. **`stream_ordering` — sequence БД** (`events_stream_ordering_seq`, `DEFAULT nextval`), тип `bigint`.
   Сквозной монотонный порядок поступления: по нему работают `/sync` (`since`), `/messages`, пагинация.
   Назначается в БД, а не приложением — исключает гонки при параллельных вставках.
2. **`depth` — топологическая глубина** (`max(prev.depth)+1`), отдельный индекс `(room_id, depth)`
   для истории/контекста. Разделение depth/stream_ordering скопировано у Synapse осознанно:
   `/messages` и `/context` — топологические, `/sync` — потоковые.
3. **`room_state` как материализация.** State-событие пишется в `events`, затем UPSERT в `room_state`
   по `(room_id, type, state_key)`. Это выполняет требование «уникальность (room_id, state_key, type)»
   и даёт O(1) чтение `GET /state/{type}/{stateKey}`.
4. **Идемпотентность через `transactions`**, а не через проверку `event_id`: клиент повторяет PUT с тем же
   `txn_id` — сервер возвращает ранее созданный `event_id` без новой вставки.
5. **Хеши вместо секретов**: `access_tokens.token_hash` (SHA-256), `users.password_hash` (argon2id PHC).
6. **JSONB для `content`** + GIN-индексы не создаём на старте (не нужны до поиска/relations).
7. **GIN по `prev_events`** — для обхода DAG без join-таблицы; на малых объёмах это дешевле `event_edges`.

### Уточнение после реализации

`room_memberships` — **материализация** текущего членства (одна строка на пару комната-пользователь),
поэтому в `0003_membership_unique` добавлен уникальный ключ `(room_id, user_id)`: без него UPSERT
невозможен. В Synapse историческое членство лежит в `room_memberships` по `event_id`, а актуальное —
в `current_state_events`; у нас роль `current_state_events` играет `room_state`, а `room_memberships`
специально хранит только актуальное значение (история — в `events`).

### Индексы и их назначение

| Индекс | Зачем |
|---|---|
| `events (room_id, stream_ordering)` | `/sync`, `/messages`, `since`-курсоры |
| `events (room_id, depth)` | история/контекст в топологическом порядке |
| `events (room_id, type, state_key) WHERE state_key IS NOT NULL` | поиск state-событий при пересчёте состояния |
| `events USING gin (prev_events)` | обход DAG-рёбер |
| `room_memberships (user_id, room_id)` | `/joined_rooms`, `/sync` (список комнат пользователя) |
| `room_memberships (room_id, membership)` | участники комнаты, рассылка |
| `transactions (user_id, device_id, endpoint, txn_id)` (PK) | идемпотентность по `txn_id` |
| `access_tokens (user_id) WHERE revoked_at IS NULL` | вход по токену, список активных сессий |

## Проверка (выполнена локально)

```
PostgreSQL 17.12 (локальный кластер, 127.0.0.1:55439)
migrate -up        → версия 2, dirty: false; таблиц 11 (10 + schema_migrations)
migrate -down 1    → версия 1 (откат только 0002_media, таблица media удалена)
migrate -down      → схема пуста (кроме schema_migrations), повторный -up → версия 2
make check         → golangci-lint 0 issues, govulncheck: no vulnerabilities
go test -tags integration ./internal/store -v
  TestMigrationsUpDown            PASS  (up → version=2 → down 1 → version=1 → down → ErrNilVersion)
  TestSchemaCoversP0              PASS  (10 таблиц P0 + ключевые индексы на месте)
  TestEventAppendAndStreamOrder   PASS  (3 события, рост stream_ordering, since-курсор, UPSERT state)
```

## Перекрёстная проверка покрытия P0 (docs/api-scope.md)

| Блок P0 | Эндпоинты | Таблицы/индексы |
|---|---|---|
| AUTH | `/register`, `/login`, `/logout`, `/whoami`, `/refresh`, `/devices*` | `users` (localpart UNIQUE, argon2id), `devices`, `access_tokens` (хеш + `refresh_hash`) |
| ROOMS | `/createRoom`, `/join`, `/invite`, `/leave`, `/state/*`, `/joined_rooms` | `rooms`, `room_aliases`, `room_state`, `room_memberships` |
| EVENTS | `/send/{type}/{txnId}`, `/messages`, `/event/{eventId}`, `/redact` | `events` (+`events_room_stream_idx`, GIN `prev_events`), `transactions` |
| SYNC | `/sync` (initial + incremental) | `events.stream_ordering`, `room_state`, `room_memberships (user_id, room_id)` |
| MEDIA | `/_matrix/client/v1/media/{upload,download,config,preview_url}` | `media` (0002), плюс алиас `/_matrix/media/v3/*` по флагу `media_legacy_v3` |

Вне этой миграции осознанно: `to_device_messages`/`device_keys`/`room_keys` (P1, E2EE - отдельные
миграции вместе с аутентификацией), `current_state_delta_stream`, федеративные кеши (федерация).

## Последствия и риски

- **Плюс:** прямые, читаемые запросы; нет state-groups-механики (самая сложная часть Synapse-хранилища);
  идемпотентность и токены безопасны по умолчанию.
- **Минус/риск:** для `/sync` с delta-состоянием понадобится таблица `current_state_delta_stream`
  (аналог Synapse) - запланировать вместе с проектированием `/sync` (ADR-0005), иначе на каждом `/sync` придётся пересчитывать state.
- **Риск:** `events.content` jsonb при очень больших событиях дороже text по TOAST; лимит размера события
  (в спеке — 65536 байт) проверим на уровне API.
- **Не покрыто сейчас:** подписи событий, auth chains, outliers, ретеншн — появятся с федерацией
  отдельными миграциями; схема к этому готова (`origin`, `auth_events`, `rejected_reason`).
