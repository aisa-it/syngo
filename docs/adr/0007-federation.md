# ADR-0007. Федерация (Server-Server API): scope, подписи, state resolution и оценка этапа федерации

- Статус: принято
- Дата: 2026-11-17
- Референс поведения: `element-hq/synapse` @ `40f63e0` (`synapse/federation/transport/server/federation.py`,
  `synapse/rest/key/v2/*`, `synapse/state/v2.py`, `synapse/federation/sender/*`), спецификация
  Server-Server API (v1.11), правила проекта — ADR-0002 (Synapse — эталон), ADR-0003 (схема), ADR-0005 (`/sync`).
- ADR фиксирует проектное решение до кода; изменения кода сервера в нем не описываются.

## 1. Зачем отдельный ADR

Федерация — самый дорогой и рискованный блок: state resolution, DAG, подписи, интероп с Synapse,
ретраи и защита от чужих серверов. Ошибка здесь не «медленно», а «пользователей выкидывает из комнат»
(урок Dendrite/Synapse state resets). Поэтому сначала фиксируем границы, модель и оценку, потом код.

## 2. Scope: что входит в этап федерации

Минимальный набор S2S-эндпоинтов, чтобы P0-сценарий работал **между двумя homeserver'ами**:
пользователь A нашего сервера создаёт комнату → приглашает пользователя B с чужого сервера →
B входит → стороны обмениваются сообщениями → история подтягивается.

| Группа | Эндпоинты (v1) | Зачем |
|---|---|---|
| Транзакции | `PUT /_matrix/federation/v1/send/{txnId}` | доставка событий (PDU/EDU) на чужой сервер; батчами, идемпотентно по `txnId` |
| Join | `GET /make_join/{roomId}/{userId}`, `PUT /send_join/{roomId}/{eventId}` | вход пользователя чужого сервера в нашу комнату (и наоборот) |
| Invite | `PUT /invite/{roomId}/{eventId}` | приглашение пользователя с чужого сервера |
| Leave / kick / ban | `GET /make_leave/…`, `PUT /send_leave/…`, `make_knock`, `send_knock` | выход, вылет, knock (room v7+) |
| Чтение состояния | `GET /state/{roomId}`, `GET /state_ids/{roomId}`, `GET /event/{eventId}`, `GET /event_auth/{roomId}/{eventId}` | догон состояния и auth-цепочки при join/backfill |
| Пропущенные события | `POST /get_missing_events/{roomId}` | закрыть дыры в DAG после простоя |
| Backfill / история | `GET /backfill/{roomId}` | подтяжка истории при первом `/messages` |
| Ключи | `GET /_matrix/key/v2/server` (+ `/{keyId}`) | публикация своих ed25519-ключей, нотариальный ответ |
| Профиль/membership-события | `GET /query/{queryType}` (`profile`, `directory`) | отображение чужих участников и алиасов |
| EDU | `m.typing`, `m.receipt`, `m.presence`, `m.direct_to_device`, `m.device_list_update`, `m.signing_key_update` в `/send` (входящие и исходящие через `federation_outbox.edu_json`) | набор, прочтение, presence, to-device и изменения устройств пользователей других серверов |
| Ключи устройств | `POST /user/keys/query`, `POST /user/keys/claim`, `GET /user/devices/{userId}` | E2EE в комнатах с участниками других серверов; кэш их устройств - `remote_device_lists`/`remote_devices` |
| Медиа | `GET /media/download/{mediaId}`, `GET /media/thumbnail/{mediaId}` (multipart/mixed) | вложения между серверами; чужое медиа кешируется (`media.is_remote`), фолбэк на `/_matrix/media/v3/download` |

**Вне scope этапа федерации** (и почему):

- P2P/Matrix 2.0, sliding sync (MSC4186), MSC-расширения федерации — не в v1.11, меняются;
- Application Service / Identity Service API — нет потребителей в закрытом контуре;
- третьи стороны (`/exchange_third_party_invite`), e-mail-инвайты — корпоративный контур их не использует;
- релей/шлюзы к другим сетям, bridges — отдельный проект;
- криптография E2EE — федерация переносит ключи устройств и to-device, но не расшифровывает (`/keys/*` и перенос между серверами).

Правило: реализуем **только** то, что нужно для P0-сценария и проверяется Complement. Всё
«на будущее» — не кодируем (иначе повторяем polylith-ошибку Dendrite).

## 3. Модель подписей

- **Ключи сервера:** ed25519, пара `key_id` (`ed25519:<version>`) + private key. Формат и публикация —
  как в Synapse: JSON с `server_name`, `valid_until_ts`, `verify_keys`, `old_verify_keys`, подпись самого
  ответа (self-signed) + подписи нотариев (notary) для кросс-проверки.
- **Хранение:** приватные ключи — только файл (`SYNGOD_FEDERATION_SIGNING_KEY`, chmod 600) или secret-хранилище;
  в БД — только `key_id`, публичная часть и `valid_until`. Ротация: новый ключ добавляется как `old_verify_keys`
  с периодом валидности (по умолчанию 7 дней), старые остаются проверяемыми до `valid_until`.
- **Проверка входящих:** заголовок `Authorization: X-Matrix origin="…",destination="…",key="ed25519:…",sig="…"`;
  проверяем: (1) `origin` — известный сервер и ключ есть в кэше/получен запросом, (2) подпись над каноническим
  JSON (`X-Matrix` + метод + URI + тело), (3) `destination` = наш `server_name`, (4) `valid_until_ts` не истёк.
- **Исходящие:** подписываем каждое PDU (по правилам room version) и HTTP-запрос; ключ — тот же.
- **Кэш чужих ключей:** таблица `server_signing_keys` (`server_name`, `key_id`, `verify_key`, `valid_until_ts`,
  `fetched_at`), TTL + перезапрос при истечении; нотариальный запрос — по списку `trusted_key_servers` из конфига.

## 4. State resolution v2

- **Порт, а не изобретение.** Берём алгоритм из `synapse/state/v2.py` (и сверяем с `ruma-state-res`, независимая
  реализация) — но код пишем свой на Go, с differential-тестами против эталона.
- **Вход:** множество state-событий (кандидатов) из `prev_events`/`auth_events` конфликтующих веток,
  auth-цепочки, power levels. **Выход:** детерминированный набор «какие `(type,state_key)` побеждают».
- **Что уже есть:** материализованный `room_state` (ADR-0003) — результат применённого state res;
  `events.prev_events`/`auth_events` — DAG-грани; `room_memberships` — история membership.
- **Что добавится:** `event_auth` (материализация auth-цепочки: `event_id`, `auth_event_id`) для быстрого
  построения auth-множества; при необходимости `state_snapshots` (снапшот состояния на событие) — только
  если профилирование покажет, что `room_state` + дельт недостаточно.
- **Тесты:** differential/fuzz против эталона (набор PDU из Complement и из реальных комнат), проверка
  детерминизма (перемешанный порядок → тот же результат), запрет state resets на стресс-тесте.
- **Правила зависят от room version:** v10/v11 (наш скоуп) — state res v2, redaction v11, auth rules v10/v11;
  v12 — state res v2.1, room_id = хеш create, create вне `auth_events`, создатели v12.
  Изменения правил — по room version, а не глобально; v1–v9 вне скоупа (ADR «api-scope»).

## 5. Транспорт и очереди

- **HTTP-клиент:** таймауты (connect 5 c, общий 30 c), retry с экспоненциальным backoff + jitter
  (старт 5 c, потолок 1 ч), счётчик неудач на хост, «залипание» хоста → пауза и метрика.
- **Очередь исходящих:** таблица `federation_outbox` (`destination`, `stream_ordering`, `pdu_json`/`edu_json`,
  `queued_at`, `attempts`, `next_attempt_at`) — транзакция отправляется батчем, удаляется после 200 OK;
  при ошибке — backoff. Это повторяет `federation_outbox` Synapse.
- **Входящие:** `federation_inbox` (`origin`, `txn_id`, `received_at`) — дедуплицикация повторных `send`
  (обязательно: чужой сервер будет ретраить).
- **Резолв адреса:** сначала `/.well-known/matrix/server` (с кэшем и таймаутом), затем SRV `_matrix-fed._tcp`,
  затем A/AAAA на 8448 и на 443 (fallback). Только HTTPS, проверка сертификата, SNI = `server_name`.
- **Ограничения:** allowlist/denylist хостов в конфиге, лимит размера тела (по умолчанию 1 MiB на транзакцию),
  лимит числа PDU в транзакции, rate limit на входящие `send` по `origin` (по образцу `rc_federation`).
- **Дискавери комнат:** `destination_rooms` (`destination`, `room_id`, `stream_ordering`) — какие комнаты
  какому серверу отправлять (аналог Synapse).

## 6. Новые таблицы (миграция `0003_federation`)

| Таблица | Назначение | Аналог в Synapse |
|---|---|---|
| `server_signing_keys(server_name, key_id, verify_key bytea, valid_until_ts, fetched_at)` | кэш чужих ключей | `server_keys_json`/`server_signature_keys` |
| `federation_outbox(destination, stream_ordering, pdu_json jsonb, edu_json jsonb, attempts, next_attempt_at)` | исходящая очередь | `federation_outbox` |
| `federation_inbox(origin, txn_id, received_at)` | дедуп входящих транзакций | `federation_inbound` (staging) |
| `destination_rooms(destination, room_id, stream_ordering)` | что и кому отправлять | `destination_rooms` |
| `destinations(destination, failure_ts, retry_last_ts, retry_interval, last_successful_stream_ordering)` | состояние ретраев по хостам | `destinations` |
| `event_auth(event_id, auth_event_id)` | auth-цепочка для state res | `event_auth` |

Существующие таблицы не меняются: `events` уже содержит `prev_events`/`auth_events`/`origin`, `room_state`
материализует состояние, `room_memberships` — историю участия.

## 7. Оценка этапа (человеко-дни, один разработчик)

| Блок | Оценка | Комментарий |
|---|---|---|
| Ключи сервера + `X-Matrix` (подпись/проверка), кэш и нотариальный запрос | 3 | криптография простая, аккуратность в каноническом JSON |
| Транспорт: HTTP-клиент, well-known/SRV, retry/backoff, allowlist, rate limit входящих | 5 | много краевых случаев и таймаутов |
| Очереди: outbox/inbox, `destination_rooms`, `destinations`, батчинг, дедуп | 5 | включает миграцию `0003_federation` |
| Join-флоу: `make_join`/`send_join`, auth-цепочка, `event_auth`, идемпотентность | 6 | самый «богатый» на ошибки сценарий |
| Invite/leave/kick/ban/knock | 3 | во многом повторяет join |
| `send`, `event`, `state`, `state_ids`, `get_missing_events`, `backfill` | 6 | DAG-обход, пагинация, лимиты |
| State resolution v2 (порт + differential/fuzz-тесты против эталона) | 10 | главный риск; без него дальше нельзя |
| Интероп: прогон Complement federation-тестов + Synapse как пир, фиксы | 6 | проверка «наш сервер ↔ Synapse», а не «сам с собой» |
| Наблюдаемость, метрики очередей/ошибок, документация, hardening | 4 | иначе федерацию невозможно эксплуатировать |
| **Итого** | **48 ч/д** (≈ 10 недель с запасом на ревью) | из них 10 ч/д — state res, 6 — интероп |

Порядок работ: ключи+транспорт → очереди → join/invite/leave → send/state/backfill → state res →
интероп. Критерий завершения этапа: интероп-прогон с эталонным Synapse и **ноль state resets** на стресс-тесте.

## 8. Риски

| Риск | Влияние | Ответ |
|---|---|---|
| Ошибка в state res → расхождение состояния, выброс пользователей | критично | порт с эталона + differential/fuzz против Synapse, запрет мержа без зелёных тестов |
| Spec drift (федерация меняется) | высокое | замороженная целевая версия спеки (v1.11), регулярная сверка с релизами Synapse подсвечивает изменения |
| Интероп «на бумаге» | высокое | обязательный прогон против реального Synapse, а не только Complement |
| DoS из федерации (большие транзакции, шторм ретраев) | среднее | лимиты размера/кол-ва, rate limit по origin, backoff с jitter |
| Утечка приватного ключа сервера | критично | только файл/secret-хранилище, ротация с `old_verify_keys`, аудит доступа |
| Сложность очередей (дубли, порядок) | среднее | `txn_id`-дедуп, `stream_ordering` как порядок, идемпотентные обработчики |
| Bus factor 1 | высокое | ADR + тесты + README «как поднять два сервера и проверить федерацию» |

## 9. Что этот ADR **не** решает

- Конкретный формат `pdu_json` (зависит от room version — фиксируется в отдельном ADR при реализации);
- политику нотариев (кого доверяем) - конфиг, решается отдельно;
- деплой/масштабирование (несколько процессов-федераторов) - позже (ADR-0009);
- E2EE-семантику (to-device, device lists) - хранение реализовано отдельно, федерация их только перевозит.
