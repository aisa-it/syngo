# Карта Matrix Client-Server API и приоритеты

Документ фиксирует скоуп, приоритеты и риски.

> **Сверка с кодом на 19.11.2026:** статусы ниже отражают фактическое состояние
> кода, а не исходный план. Готово: auth (регистрация/логин/refresh/logout/password/deactivate/
> delete_devices), комнаты (create/join/invite/leave/kick/ban/unban/send/messages/state/redact),
> /sync (long-poll, to-device, account data, device lists), ключи (/keys/*), медиа (upload/
> download/preview_url), pushers, /search, /register/available, федерация
> (key server, X-Matrix, send/state/backfill/gap fill, invite/join/leave, send_join/leave v1+v2,
> state resolution v2), метрики и graceful shutdown, `/capabilities`, `/joined_rooms`, фильтры `/sync`,
> push rules и служебные эндпоинты для Element, simplified sliding sync для Element X.
> Не начато: thumbnails, полнотекстовый поиск (сейчас ILIKE по body).
> Колонка «покрыт Complement» не выставляется, пока baseline не зелёный.

**Мы портируем Synapse**, поэтому он остаётся постоянным эталоном поведения: контракт объясняет
спецификация **Client-Server API v1.11** (<https://spec.matrix.org/v1.11/client-server-api/>), а как это
происходит в реальности — показывает Synapse. При расхождении по умолчанию воспроизводим поведение
Synapse (совместимость клиентов важнее чистоты) и фиксируем решение в ADR с датой пересмотра.
Подробно - ADR-0002.

- Целевая версия спеки: **v1.11** (заморожена на релизный цикл; обновление - отдельным решением).
- Комнаты: цель — **room version 10** (по умолчанию в `createRoom`), **v11** (решение от 17.11.2026)
  и **v12** для клиентов, переноса из Synapse 1.165 и федерации;
  v1–v9 — вне скоупа, совместимость с v9 исследуется отдельно (миграция, ADR-0006).
- Формат статуса: `не начат` → `в работе` → `готов` → `покрыт Complement`.
- Оценка сложности: **S** ≤ 1 дня, **M** 2–4 дня, **L** ≥ недели (для одной задачи).

## 1. P0 — MVP (сквозной сценарий)

Сценарий, который обязан проходить без Synapse: *клиент логинится → создаёт комнату → приглашает
второго пользователя → обменивается сообщениями → получает их через `/sync`*.

| Эндпоинт | Назначение | Слож. | Статус | Риск / что учесть | Спека |
|---|---|---|---|---|---|
| `GET /_syngo/v1/source` | лицензия и ссылка на соответствующий исходный код | S | готов | публично даже при выключенной федерации; `COMMIT` должен указывать на опубликованную ревизию | AGPL §13, `NOTICE` |
| `GET /_matrix/client/versions` | список поддерживаемых версий CS-API | S | готов | список версий = контракт с клиентом, расширять только вперёд | [#api-versions](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientversions) |
| `GET /_matrix/client/v3/capabilities` | возможности сервера (room versions, join rules) | S | готов | клиенты проверяют перед `createRoom`; отдавать реальный список room versions | [#capabilities](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3capabilities) |
| `POST /_matrix/client/v3/register` | регистрация пользователя | M | готов | UIA-флоу, rate-limit, argon2id, выдача device/access-token; капча — провайдером | [#registration](https://spec.matrix.org/v1.11/client-server-api/#registration) |
| `POST /_matrix/client/v3/login` | вход по `m.login.password` | M | готов | refresh tokens, `device_id`, `well_known`, одинаковый ответ на неверный логин/пароль | [#login](https://spec.matrix.org/v1.11/client-server-api/#login) |
| `POST /_matrix/client/v3/logout`, `POST /logout/all` | выход (текущее/все устройства) | S | готов | инвалидация токенов в БД, а не в памяти | [#logout](https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3logout) |
| `GET /_matrix/client/v3/account/whoami` | кто владелец токена | S | готов | нужен клиентам на старте и для отладки; `device_id` в ответе | [#whoami](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3accountwhoami) |
| `POST /_matrix/client/v3/createRoom` | создание комнаты + начальное состояние | L | готов | genesis-события (`m.room.create/power_levels/member`), presets, `initial_state`, `room_version`, alias | [#createRoom](https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv3createroom) |
| `POST /_matrix/client/v3/join/{roomIdOrAlias}`, `POST /rooms/{roomId}/join` | вход в комнату | M | готов | membership-переходы и power levels; резолв alias; идемпотентность | [#joining](https://spec.matrix.org/v1.11/client-server-api/#joining-rooms) |
| `POST /_matrix/client/v3/rooms/{roomId}/invite` | приглашение пользователя | M | готов | право `invite` по power levels, `m.room.member` с `invite`, отображение в `/sync` приглашённого | [#inviting](https://spec.matrix.org/v1.11/client-server-api/#inviting-joining) |
| `POST /rooms/{roomId}/leave` | выход из комнаты | S | готов | `leave`/`knock`-переходы, уведомление остальных через `/sync` | [#leaving](https://spec.matrix.org/v1.11/client-server-api/#leaving-rooms) |
| `PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId}` | отправка события | L | готов | **ядро DAG**: `prev_events`, `depth`, `stream_ordering`, идемпотентность по `txnId`, auth rules | [#send](https://spec.matrix.org/v1.11/client-server-api/#put_matrixclientv3roomsroomidsendeventtypetxnid) |
| `GET /_matrix/client/v3/sync` | initial + incremental синхронизация, long-poll | L | готов | **главный риск проекта**: `since`-токены, дельты состояния, `timeout`, lazy-loading, отсутствие N+1 по комнатам | [#sync](https://spec.matrix.org/v1.11/client-server-api/#syncing) |
| `GET /_matrix/client/v3/rooms/{roomId}/messages` | история с пагинацией | M | готов | токены `from`/`to`, `dir`, порядок (stream vs topological), лимиты | [#messages](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3roomsroomidmessages) |
| `GET /_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}` | чтение состояния | S | готов | 404 с `M_NOT_FOUND` для отсутствующего state, кеш материализованного state | [#state](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3roomsroomidstateeventtypestatekey) |
| `GET /_matrix/client/v3/joined_rooms` | список комнат пользователя | S | готов | запрос по `room_memberships` (membership=join) | [#joined_rooms](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv3joined_rooms) |
| `POST /_matrix/client/v1/media/upload` | загрузка медиа | M | готов | лимиты размера, MIME-валидация, хранение (локально/S3), `content_uri` | [#upload](https://spec.matrix.org/v1.11/client-server-api/#post_matrixclientv1mediaupload) |
| `GET /_matrix/client/v1/media/download/{serverName}/{mediaId}[/{fileName}]` | скачивание медиа | S | готов | авторизация по `mediaId`, `Content-Disposition`, защита от path traversal | [#download](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediadownloadservernamemediaidfilename) |
| `GET /_matrix/client/v1/media/preview_url`, `GET /_matrix/client/v1/media/config` | метаданные медиа | S | в работе (preview_url готов, config нет) | конфиг лимитов отдаётся клиенту до загрузки | [#config](https://spec.matrix.org/v1.11/client-server-api/#get_matrixclientv1mediaconfig) |

**Порядок работ по P0** (зависимости): `AUTH` (register/login/whoami/logout) → `ROOMS`
(createRoom/join/invite/leave/state) → `EVENTS` (send/messages) → `SYNC` → `MEDIA`.
`/versions` и `/capabilities` уже/сразу — они нужны для smoke-тестов.
`MEDIA` - последним блоком P0 (решение от 17.11.2026): MVP не блокирует, миниатюры вынесены в P1.

## 2. P1 — сразу после MVP

| Группа | Эндпоинты | Слож. | Риск |
|---|---|---|---|
| Ключи и устройства (E2EE-хранение, без серверной криптографии) | `POST /keys/upload`, `POST /keys/query`, `POST /keys/claim`, `POST /keys/device_signing/upload`, `POST /keys/signatures/upload`, `GET /keys/changes` — **готово** | M | объёмы и TTL one-time keys, отдача «как есть»; сервер не расшифровывает |
| Бэкап ключей комнат | `GET/PUT /room_keys/version`, `PUT/GET/DELETE /room_keys/keys…` | M | большие блобы, версионирование, права только владельцу |
| Устройства | `GET /devices`, `GET/PUT/DELETE /devices/{deviceId}` | S | удаление устройства = инвалидация токенов |
| Профиль | `GET /profile/{userId}`, `/displayname`, `/avatar_url` (+PUT) | S | кеш профилей, приватность (`membership`-видимость) |
| Presence / typing / receipts | `PUT /presence/{userId}/status`, `PUT /rooms/{roomId}/typing/{userId}`, `POST /rooms/{roomId}/receipt/{type}/{eventId}` | M | эфемерные события: не в DAG, доставка через `/sync`, TTL |
| Миниатюры медиа | `GET /_matrix/client/v1/media/thumbnail/{serverName}/{mediaId}` | M | ресайз (imaging-пакет), кеш, лимиты CPU - вынесено из P0 (раздел 7) |
| Легаси-алиас медиа | `/_matrix/media/v3/{upload,download,thumbnail,preview_url,config}` | S | **решение:** тонкий алиас на v1-реализацию (те же хендлеры, другой префикс), флаг `media_legacy_v3` включён по умолчанию; старые клиенты и мосты ходят в v3 |
| Алиасы комнат | `GET/PUT/DELETE /directory/room/{alias}` — **готово** | S | уникальность, права, `m.room.aliases`-эквивалент |
| Участники комнаты | `GET /rooms/{roomId}/members`, `GET /rooms/{roomId}/joined_members` — **готово** | S | нужны клиентам для списка участников; без lazy-loading дорого на больших комнатах |
| Событие и алиасы комнаты | `GET /rooms/{roomId}/event/{eventId}`, `GET /rooms/{roomId}/aliases` — **готово** | S | прямое чтение события, список алиасов комнаты |
| To-device сообщения | `PUT /sendToDevice/{eventType}/{txnId}` — **готово** | M | очередь `device_inbox`, доставка в `/sync` `to_device.events` с удалением после выдачи, идемпотентность по `txnId` |
| Refresh токена (явный) | `POST /refresh` — **готово** | S | обновление access-token по refresh-token |
| Доступность логина | `GET /register/available` — **готово** | S | проверка занятости localpart до регистрации |
| Массовое удаление устройств | `POST /delete_devices` — **готово** | S | UIA-подтверждение, инвалидация токенов пачкой |
| User/room account data | `PUT/GET /user/{userId}/account_data/{type}`, `.../rooms/{roomId}/account_data/{type}` | S | произвольный JSON, приватность |
| Теги, read markers | `GET/PUT/DELETE /user/{userId}/rooms/{roomId}/tags…`, `POST /rooms/{roomId}/read_markers` | S | — |
| Фильтры | `GET/POST /user/{userId}/filter`, `GET /user/{userId}/filter/{filterId}` — **готово** | M | хранится в `user_filters`, нужен `/sync` (ленивая загрузка участников) |
| Контекст события | `GET /rooms/{roomId}/context/{eventId}` | M | выборка соседей по DAG |
| Redaction | `PUT /rooms/{roomId}/redact/{eventId}/{txnId}` | M | правила redaction зависят от room version |

### Simplified sliding sync (MSC4186)

Element X (iOS, Android) синхронизируется **только** через simplified sliding sync: без него
мобильный клиент показывает «Сервер недоступен» (при переключении с Synapse - 404 на
`POST .../simplified_msc3575/sync`). Поэтому протокол в скоупе, хотя в CS-API v1.11 его нет.
Эталон поведения - Synapse 1.165 (`synapse/handlers/sliding_sync`, `SlidingSyncRestServlet`).

| Что | Статус | Как сделано |
|---|---|---|
| `POST /_matrix/client/unstable/org.matrix.simplified_msc3575/sync` (`pos`, `timeout`; `conn_id`, `lists`, `room_subscriptions`, `extensions`) | готов | `core.SlidingSyncService` + `internal/api/sliding_sync.go`; лимит и long-poll - как у `/sync` |
| Объявление поддержки | готов | `/versions`: `unstable_features["org.matrix.simplified_msc3575"] = true` (по нему Element X включает протокол) |
| Списки: `ranges`, `count`, операция `SYNC` | готов | сортировка по `bump_stamp` (последнее из `m.room.create`, `m.room.message`, `m.room.encrypted`, `m.sticker`, `m.call.invite`, `m.poll.start`, `m.beacon_info`; не-join - событие членства), при равенстве - `room_id` |
| Фильтры: `is_dm` (`m.direct`), `is_encrypted`, `is_invite`, `room_types`/`not_room_types` (`null` - без типа), `spaces`, `tags`/`not_tags` | готов | `room_name_like` не поддержан (как в Synapse); `spaces` - только прямые `m.space.child` пространств с членством |
| Комната: `name`, `avatar`, `heroes`, `initial`, `required_state`, `timeline`, `prev_batch`, `limited`, `num_live`, `bump_stamp`, `joined_count`, `invited_count`, `notification_count`, `highlight_count`, `is_dm`, `invite_state`, `unstable_expanded_timeline` | готов | пустые поля не пишутся, как Synapse; счетчики непрочитанного - реальные (Synapse отдает нули) |
| `required_state`: типы/ключи, `*`, `["*","*"]` (все состояние), `$LAZY`, `$ME`, изменение между запросами | готов | при расширении дочитывается текущее состояние новых ключей; lazy-участники, которых клиент уже получил, повторно не отдаются |
| Состояние соединения (user, device, `conn_id`), `pos`, `M_UNKNOWN_POS` | готов | миграция 0047: `sliding_sync_connections`, `sliding_sync_connection_positions` (состояние позиции - jsonb); статусы LIVE/PREVIOUSLY по комнатам, account data и receipts, параметры комнат, lazy-участники; соединения без запросов 7 дней удаляются |
| `room_subscriptions`, `unsubscribe_rooms` | готов | подписки «липкие»: хранятся в состоянии соединения до `unsubscribe_rooms` (matrix-js-sdk присылает подписку один раз; Synapse так не умеет). Комната без членства отдается, только если ее можно просмотреть до входа (`join_rules` public или `world_readable`): stripped state без timeline, один раз - иначе Element Web висит на «Loading...» |
| Расширения `to_device` (MSC3885), `e2ee` (MSC3884), `account_data` (MSC3959), `receipts` (MSC3960), `typing` (MSC3961), фильтр `lists`/`rooms` | готов | данные - те же сервисы, что у `/sync` |
| Расширения `io.element.msc4308.thread_subscriptions`, `org.matrix.msc4354.sticky_events`, профили (MSC4262); `knock_state`; липкие параметры списков | не начат | Synapse первые три держит за экспериментальными флагами; клиенты без них работают |

## 3. P2 - после P1 / по запросу

| Группа | Эндпоинты | Почему P2 |
|---|---|---|
| Relations, агрегации, треды | `GET /rooms/{roomId}/relations/…`, `/aggregations/…`, `/threads` | нужны современным клиентам, но не для сквозного сценария; требуют материализации агрегатов |
| Поиск | `POST /search` | нужен полнотекстовый индекс (PostgreSQL FTS) — отдельный компонент |
| Push / уведомления | `GET/PUT /pushers` - готово; `/pushrules/*` (GET/PUT/DELETE, `/enabled`, `/actions`, `before`/`after`, полный набор серверных правил) - готово; вычисление правил для push, `unread_notifications` в `/sync` и `GET /notifications` - готово | требует push-gateway |
| Голос/видео | `GET /voip/turnServer` | TURN отдаём внешним coturn, сервер только выдаёт креды |
| Третьи стороны / OpenID / гости | `/thirdparty/*`, `POST /openid/request_token`, guest-регистрация | не нужны для корпоративного контура |
| Публичный каталог (read-only) | `GET /publicRooms` | решение от 17.11.2026: только чтение; `POST /publicRooms` и `/directory/list/room/{roomId}` не делаем |
| Пространства (spaces) | `GET /rooms/{roomId}/hierarchy` | надстройка над relations/state; после тредов |
| Upgrade комнаты | `POST /rooms/{roomId}/upgrade`, `m.room.tombstone` | редкий сценарий, требует аккуратной миграции состояния |

## 4. Осознанно вне скоупа

| Что | Почему |
|---|---|
| Серверное шифрование сообщений (`m.megolm` расшифровка на сервере) | противоречит модели E2EE Matrix; сервер хранит и раздаёт ключи, но не расшифровывает |
| Federation (Server-Server API) | отдельная спецификация и отдельный этап (ADR-0007); MVP - один сервер. Влияние на схему БД учитывается сразу (`origin`, `prev_events`) |
| TURN/STUN, звонки | инфраструктурная задача (coturn), не код homeserver'а |
| Admin API Synapse (`/_synapse/admin/*`) | не входит в спеку; будет собственный admin-контур `/_syngo/admin/v1/*` — формат зафиксирован в [ADR-0008](adr/0008-admin-api.md) |
| Application Service API, Identity Service API | нет потребителей в корпоративном контуре; добавлять по запросу |
| `initialSync`, `/events`, `r0`-версии путей | deprecated, удалены/не рекомендуются; поддерживаем только `/v3`-префиксы |
| Гостевой доступ | вычеркнут 17.11.2026: в закрытом контуре не нужен |
| Клиентская часть (web/desktop клиент) | не наш скоуп: совместимость проверяем Complement и живыми клиентами (Element) |

## 5. Сверка с исходниками Synapse

Инвентаризация сделана по исходникам `element-hq/synapse`, shallow-клон, коммит **`40f63e0`**
(17.11.2026): `synapse/rest/client/*.py`, извлечение `client_patterns(...)` / `PATTERNS`
(123 маршрута в 45 модулях). Выводы, повлиявшие на документ:

1. **Медиа**: в CS-API v1.11 «домашний» путь — `/_matrix/client/v1/media/*`, а `/_matrix/media/v3/*`
   помечен deprecated. Основной путь — v1; легаси v3 — тонкий алиас (P1, флаг `media_legacy_v3`).
2. **To-device сообщения** (`/sendToDevice/*`) и **`/keys/changes`** — Synapse держит их в P1-наборе;
   без `/sendToDevice` E2EE-клиенты не работают, поэтому он в P1, а не P2.
3. **Список участников** (`/rooms/{id}/members`, `/joined_members`) и `/rooms/{id}/aliases` появились
   в документе как P1 после сверки — в исходной карте их не было.
4. **Префиксы версий**: Synapse регистрирует один хендлер на `/v1`, `/v3` и `unstable` через
   `client_patterns(..., v1=True)`, txn-эндпоинты добавляет `register_txn_path()`. У нас: `/v3` для
   CS-API, `/v1` для media, `/{txnId}` — у всех идемпотентных PUT.
5. **Synapse-специфика, не переносим:** `/password_policy`, `/mutual_rooms`, `/user_directory/search`,
   MSC-эндпоинты (delayed events 4140, dehydrated devices 3814,
   threads 4306/4308, rendezvous 4108/4388, auth metadata 2965). Исключение - simplified sliding
   sync (MSC4186, путь `org.matrix.simplified_msc3575`): его в v1.11 нет, но без него не работает
   Element X, поэтому он перенесен (раздел 2).
6. **OpenID**: у Synapse `POST /user/{userId}/openid/request_token`, в спеке `POST /openid/request_token`.
   Делаем по спеке; при необходимости совместимости добавим оба (P2).

## 6. Что влияет на другие части

- **Схема БД (ADR-0003)** должна сразу предусматривать: `events(prev_events[], depth, stream_ordering)`,
  `room_state` (материализованный state), `access_tokens` (хеши), `room_memberships`, `device_id`,
  `txn_id`-идемпотентность, `media`-метаданные и `room_keys`-блобы.
- **Complement**: baseline снимаем по группам CS-API — P0 целиком, затем P1 по мере готовности.
- **`/sync` SLO (ADR-0005)**: цель p99 и профиль нагрузки фиксируются до реализации `/sync`,
  чтобы не оптимизировать постфактум (урок Dendrite).

## 7. Решения по скоупу (17.11.2026)

1. `MEDIA` остаётся в P0 последним блоком — MVP не блокирует; миниатюры → P1.
2. Room versions: **v10 + v11 + v12** (v12 - клиенты, перенос из Synapse и федерация); v9 - только исследование (ADR-0006).
3. Гостевой доступ вычеркнут; `publicRooms` — только read-only и в P2.
4. Легаси медиа `/_matrix/media/v3/*` **держим**: тонкий алиас на v1-реализацию, P1, флаг
   `media_legacy_v3` (по умолчанию включён). Открытых вопросов по скоупу нет.

### Управление состоянием и модерация

- `PUT /_matrix/client/v3/rooms/{roomId}/state/{eventType}/{stateKey}` и вариант без
  `stateKey` (пустой ключ) — state-событие с auth-проверкой по текущему состоянию
  (`state_default` / `events[type]`), запись в DAG, `room_state`, fan-out в федерацию,
  ответ `{event_id}`. Идемпотентность — по хэшу тела.
- `GET /rooms/{roomId}/state` — всё материализованное состояние комнаты;
  `GET /rooms/{roomId}/state/{eventType}/{stateKey}` — `content` одного ключа.
- `POST /rooms/{roomId}/kick`, `/ban`, `/unban` — `m.room.member` с проверками:
  нельзя себя, нельзя равного по уровню, ban требует `ban`-уровень, unban — тоже
  (цель в состоянии `ban`), kick — цель должна быть `join`. Ответ `{}` при успехе.
- `PUT /rooms/{roomId}/redact/{eventId}/{txnId}` — `m.room.redaction` + применение
  правил redaction v10/v11: content вырезается (для `m.room.member` остаётся
  `membership`, для `m.room.power_levels` — уровни, для `m.room.create` — `creator`),
  остальные типы теряют content целиком. Автор события может всегда, остальным нужен
  уровень `redact`; само событие требует уровня отправки `events["m.room.redaction"]`
  (иначе `events_default`).
- Применение redaction: пометка `events.redacted_by` и вырезание `content`
  цели по правилам версии комнаты выполняются в транзакции записи события
  `m.room.redaction` (локальной и федеративной). Redaction, пришедшая раньше цели,
  хранится в таблице `redactions` и применяется при записи цели. Повторная redaction
  не переписывает `redacted_by`. Чужой сервер скрывает событие, только если домены
  отправителей совпадают или у отправителя есть уровень `redact`.
- В `events.content` после redaction лежит вырезанный content (его видят клиенты,
  поиск и связи); оригинал - в `redactions.original_content`, стирается по
  `redaction.retention_period` (по умолчанию 7 дней, как `redaction_retention_period`
  в Synapse; 0 - не сохраняется). События, удаленные до миграции 0029, вырезаются
  при старте syngod.
