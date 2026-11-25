# ADR-0006. Миграционный путь с Synapse: что переносим, чем и по какой цене

- Статус: предложено (spike); решение по откату - §7; комнаты v12 - §11
- Дата: 2026-11-17
- Референс: `element-hq/synapse` @ `40f63e0` — `synapse/storage/schema/main/full_schemas/72/full.sql.postgres`,
  `synapse/rest/admin/rooms.py`, `synapse/rest/admin/users.py`, `synapse/media/filepath.py`,
  `synapse/config/repository.py`, `synapse/handlers/auth.py`.
- Связанные документы: ADR-0002 (Synapse — эталон), ADR-0003 (наша схема).

## 1. Зачем

Go-реализации Matrix не приживались не из-за API, а из-за невозможности приехать с Synapse и уехать
обратно. Задача — не «написать миграцию», а зафиксировать реальный путь, его ограничения и цену,
чтобы этап миграции планировался по фактам.

## 2. Что есть на стороне Synapse

| Данные | Где в Synapse | Формат |
|---|---|---|
| пользователи | `users(name, password_hash, deactivated, admin, creation_ts)` | `name` = полный `@local:server`, хеш пароля — **bcrypt** |
| комнаты | `rooms(room_id, is_public, creator, room_version)` | `room_version` — текст |
| членство (текущее) | `room_memberships(event_id, user_id, room_id, membership, forgotten)` + `current_state_events` | по событию на строку |
| события | `events(...)` (метаданные: `stream_ordering`, `topological_ordering`, `depth`, `sender`, `type`, `state_key`, `origin_server_ts`) + `event_json(json)` | тело события — JSON-текст в `event_json.json` |
| состояние | `current_state_events(room_id, type, state_key, event_id)` | указатели на события |
| медиа (метаданные) | `local_media_repository(media_id, media_type, media_length, upload_name, user_id, created_ts, quarantined_by)` | файлы на диске |
| медиа (файлы) | `<media_store_path>/local_content/<media_id[0:2]>/<media_id[2:4]>/<media_id[4:]>`; миниатюры — `local_thumbnails/…` | blob |
| account data, E2EE-ключи | `account_data`, `room_keys`, `device_inbox`, `devices` | JSON/ключи |

Полного экспортного API у Synapse **нет**. Admin API (`/_synapse/admin/v1/rooms`, `/rooms/{id}`,
`/rooms/{id}/members`, `/rooms/{id}/hierarchy`, `/users`, `/users/{id}`) отдаёт метаданные и списки,
но не историю событий и не медиа-байты. Значит практический путь — **чтение БД Synapse** (реплика,
read-only роль) плюс копирование каталога `media_store`.

## 3. Формат передачи: JSONL-дамп, а не SQL-to-SQL

Решение: **промежуточный JSONL**, по строке на объект, без зависимости от схемы Synapse:

```json
{"kind":"user","user_id":"@alice:example.org","localpart":"alice","password_hash":"$2b$12$…","display_name":"Alice","deactivated":false}
{"kind":"room","room_id":"!abc:example.org","room_version":"10","creator":"@alice:example.org","is_public":false}
{"kind":"event","room_id":"!abc:example.org","event_id":"$e1","type":"m.room.message","sender":"@alice:example.org","state_key":null,"content":{"body":"привет"},"origin_server_ts":1700000000000,"depth":5,"prev_events":["$e0"],"stream_ordering":42}
{"kind":"membership","room_id":"!abc:example.org","user_id":"@alice:example.org","membership":"join","event_id":"$e2"}
```

Почему не SQL-to-SQL: у нас другая модель (ADR-0003) — `content` в jsonb, `stream_ordering` своим
sequence, `room_memberships` материализовано, `room_state` — снапшот. Прямой перенос схемы привязал бы
нас к версии Synapse и сделал бы необратимым любой её ALTER. JSONL же можно получить и из дампа БД,
и из admin API (для метаданных), и из будущего экспорта сторонних инструментов.

Скрипт-экспортёр — `tools/synapse-export/export.sql` (запросы `row_to_json` по таблицам выше) +
`pg_dump`/`psql -At` для сборки файла; медиа копируется `rsync` по `media_store/local_content`.

## 4. Что переносим

| Объект | Как | Потери |
|---|---|---|
| пользователи (localpart, профиль из `profiles`, deactivated, admin) | создаём в `users` с сохранением `user_id`; повторный импорт обновляет профиль и флаги, argon2id-хеш не затирает | `locked` - как deactivated (блокировки в SYNGO нет); `suspended`, `shadow_banned` - без аналога, в отчете; `user_type`/consent - вне скоупа |
| устройства и токены | `devices` (имя, last_seen), `access_tokens`: открытый токен Synapse → SHA-256, refresh - парой, `valid_until_ms` сохраняется; клиенты остаются в сессии | токены без устройства (admin API) |
| E2EE | ключи устройств, one-time keys (невыданные), fallback keys (с `used`), кросс-подписи (действующие по `stream_id`), подписи `e2e_cross_signing_signatures`, недоставленные `device_inbox` | скрытые устройства Synapse (это ключи кросс-подписи, переносятся отдельно) |
| бэкап ключей | `room_keys_versions` (в том числе удаленные - номер не переиспользуется, etag), `room_keys` | — |
| настройки клиента | `account_data` (глобальная, включая секреты SSSS), комнатная и теги (`m.tag`), push rules (порядок, enabled, переопределения серверных), pushers (включенные), фильтры с прежним `filter_id`, профили | — |
| пароли | **bcrypt → argon2id** пере-хешированием при первом входе (добавим адаптер проверки bcrypt) либо принудительный сброс; прямой перенос невозможен — форматы разные | пользователь либо один раз логинится по старому паролю (адаптер), либо проходит сброс |
| комнаты (room_id, версия, создатель, публичность) | `rooms`; версии 1-12, в том числе v12 с room_id без домена (§11); прочие версии - ошибка импорта | `has_auth_chain_index` не переносим (оптимизация федерации) |
| события + история | `events` с сохранением `event_id`, `stream_ordering` (включая backfill < 0), `depth`, `origin_server_ts`, исходного PDU (`pdu_json`: подписи, hashes, unsigned, origin, redacts), `event_auth`; отклоненные, soft-failed и outlier - в `excluded_events` вне timeline и состояния | `topological_ordering` не храним (вычисляется) |
| redactions | цель помечается `redacted_by`, content и PDU вырезаются, оригинал - в `redactions.original_content` до `redaction.retention_period` | оригинал, уже стертый Synapse (`redaction_retention_period`) |
| текущее состояние | `room_state` из `current_state_events` (строки `kind=state`), не пересчетом | `state_groups*` не переносим (нам не нужны) |
| членство | `room_memberships` (материализация), история — в `events` | `forgotten` переносим флагом |
| медиа | метаданные → `media`, файлы → storage (`local_content/<2>/<2>/<rest>`), sha256 сверяется с `local_media_repository.sha256`, карантин (`quarantined_by`) переносится и блокирует медиа | миниатюры строятся заново по запросу; превью ссылок (`url_cache`) и `remote_media_cache` не переносятся (кеш; федерация вне скоупа) |
| алиасы комнат | `room_aliases` (kind `room_alias`) | — |

## 5. Что не переносим (и почему это осознанно)

- **E2EE-история не теряется**: сервер ключи не расшифровывает, но хранит и раздает их как есть -
  устройства, ключи, кросс-подписи, бэкап ключей и секреты SSSS переносятся (§4), клиент
  остается в сессии со своим крипто-хранилищем. Прежняя формулировка «ограничение Matrix» была
  неверной. Не переносятся `device_lists*` (поток изменений): клиент после переезда делает initial
  sync (since-токен Synapse принимается как начало) и заново запрашивает ключи.
- **Федеративные кэши** (`destination_rooms`, `device_lists_outbound_*`, `federation_*`) — пересоздаются.
- **SSO** (`user_external_ids`) — провайдера OIDC нет (ADR-0010).
- **Ретеншн, статистика, background updates, `cache_invalidation_stream`** — внутренние механизмы Synapse.
- **OpenID/3PID/application services** — вне скоупа MVP.

## 6. PoC

`cmd/syngo-import` — импорт JSONL в нашу схему: `--dsn`, `--file`, `--dry-run`, `--limit`, `--server-name`.
Идемпотентен (`ON CONFLICT DO NOTHING`), сохраняет `event_id` и `stream_ordering`, обновляет `room_state`
и `room_memberships`. Проверка: комната из дампа читается через `GET /rooms/{roomId}/messages` на
поднятом `syngod` (см. раздел «Проверка» ниже и README).

Реализация по блокам этапа миграции: блок 1 — импорт пользователей и переход паролей
bcrypt→argon2id; блок 2 — комнаты, алиасы, состояние, членство (с флагом `forgotten`),
батчи с прогрессом, отчёт `import-report.json` со счётчиками и ошибками на строку (импорт при ошибке
строки не останавливается). Ограничения: один процесс, медиа-байты копируются отдельно (rsync).

## 7. Обратный путь (exit path) и откат

Чтобы не повторять ошибку Synapse, наш экспорт делается нами же:

- `syngo-import` читает тот же JSONL-формат; экспортёр `cmd/syngo-export` формирует его из наших таблиц
  в формате `syngo-synapse-export`: исходный PDU (`pdu_json`), отклоненные/soft-failed/outlier
  (`excluded_events`), оригиналы удаленных событий (`redactions`), текущее состояние (`kind=state`),
  манифест, алиасы, членство с `event_id`, медиа с карантином и sha256. Round-trip
  (`TestRoundTripSynapseFixture`): Synapse → SYNGO → `syngo-export` → SYNGO, сверка с тем же Synapse - `OK`.
- Пароли: argon2id PHC → bcrypt невозможен; при экспорте в Synapse пользователи проходят сброс пароля
  либо логинятся через SSO. Это ограничение форматов, а не потери данных.
- Медиа: обратное копирование `media_store`-совместимого каталога (наш storage_key уже хранит путь).
- Гарантия: полный дамп можно собрать без чтения Synapse-схемы, только нашими запросами.

### Решение по откату

**Односторонний переезд с окном приемки, без двойной записи.** Двойная запись (Synapse и SYNGO
одновременно) отвергнута: у двух серверов с одним `server_name` расходятся DAG, `stream_ordering`,
состояние комнат и устройства - согласовать их сложнее, чем сам переезд, а ошибка согласования
незаметна до отката.

| Этап | Откат | Потери |
|---|---|---|
| Окно простоя: Synapse остановлен, импорт, сверка, приемка (`run.sh synapse`), запись в SYNGO выключена | вернуть прокси на Synapse и запустить его: Synapse не менялся, БД SYNGO выбрасывается | нет |
| После включения записи в SYNGO (точка невозврата без потерь) | вернуть прокси на Synapse (остановлен, БД цела 24-72 ч); созданное в SYNGO после переключения - `syngo-export -after-stream <max stream_ordering импорта> -media-since <время cutover>`: архив событий и медиа для ручного разбора (переслать пользователям, восстановить важное вручную) | в Synapse не возвращаются события, медиа, входы и смена паролей, сделанные в SYNGO после переключения |

Загрузчика JSONL в Synapse нет и он не планируется: запись в таблицы Synapse в обход его кода
(`state_groups`, `event_edges`, потоки, кеши) - путь к порче БД Synapse. Если обратная загрузка
понадобится, ее делать через Application Service API Synapse (отправка от имени пользователей с
`ts`), с новыми `event_id` - отдельная работа.

Критерии отката (решение принимает ответственный за миграцию до включения записи): любая проверка `run.sh synapse`
не прошла (`syngo-verify-migration` не `OK`, вход, `/messages`, `/sync`, медиа, sequence); импорт с
ненулевым кодом, который не объясняется отчетом; окно простоя превысило объявленное. После включения
записи откат - только при неработоспособности сервиса, с потерей перечисленного выше. Порядок - в
`docs/migration-runbook.md` §6, репетиция - `docs/migration-rehearsal.md`.

## 8. Оценка этапа миграции (человеко-дни)

| Работа | Дни |
|---|---|
| пользователи + адаптер bcrypt (или сценарий сброса) + тесты | 3 |
| комнаты, состояние, членство | 3 |
| история событий (включая сохранение `stream_ordering`, батчи, валидация) | 5 |
| медиа (метаданные + файлы + thumbnails на первом запросе) | 3 |
| экспорт (exit path) + round-trip тест | 3 |
| приёмка: прогон на копии реального профиля, cutover/rollback-план, документация | 3 |
| **Итого** | **20** |

Итог совпадает с исходной оценкой spike («20 ч» - оценка исследования, не реализации);
для планирования этапа миграции - **≈20 человеко-дней** с точностью ±4 дня. Риск-буфер: media и
история зависят от объёма (`events` и `media_store` на порядки больше остальных таблиц).

## 9. Проверка PoC

1. Импорт тестового дампа (пользователь, комната, 5 событий, членство) в чистую БД.
2. `GET /rooms/{roomId}/messages?dir=b` отдаёт события в исходном порядке с исходными `event_id`.
3. Повторный импорт не создаёт дублей.

## 10. Риски

| Риск | Ответ |
|---|---|
| Долгое чтение `events` на живой реплике Synapse | читать батчами по `stream_ordering`, только read-only роль, окно обслуживания для cutover |
| bcrypt-хеши нельзя перенести 1:1 | адаптер проверки bcrypt + пере-хеширование при первом входе; fallback — сброс пароля |
| Потеря E2EE-истории при переезде | ключи, кросс-подписи, бэкап и сессии переносятся; Synapse на время миграции - read-only, иначе новые ключи после дампа потеряются |
| Медиа-хранилище в S3 | этап миграции поддерживает S3-провайдер, PoC — только локальный каталог |

## 11. Комнаты версии 12

Synapse 1.165 создает комнаты v12 по умолчанию (в том числе при замене комнаты через `/upgrade`),
поэтому на исходном сервере они есть почти наверняка. Правила v12 (MSC4289, MSC4291, MSC4297):
room_id - «!» и reference hash `m.room.create` без серверной части, у самого create нет `room_id`,
create не входит в `auth_events`; создатели - отправитель create и `content.additional_creators`,
их уровень бесконечен и в `power_levels.users` не пишется (событие с ними отвергается), tombstone
по умолчанию требует 150; state resolution v2.1. Redaction и формат `event_id` - как в v11.

**Решение: полная поддержка v12 для клиентского API и переноса (вариант «а»), федерация v12 -
отказ (позже пересмотрено: Synapse 1.165 создает v12 по умолчанию, и без федерации v12 его
пользователь не может пригласить нашего в новую комнату).** Безопасный отказ (вариант «б») отвергнут: комнаты v12 - это все новые комнаты и все
замененные комнаты источника, отказ от них означал бы отказ от переезда. Правила v12 для одного
сервера невелики: поле room_id, состав auth_events и уровень создателей; state resolution на одном
сервере не нужна (DAG линеен, состояние берется из `current_state_events`).

| Что | Где |
|---|---|
| CHECK `rooms_room_id_format` допускает `^![A-Za-z0-9_-]{43}$` | миграция `0036_rooms_room_id_v12` (откат - прежний CHECK с `NOT VALID`, перенесенные комнаты не трогает) |
| auth rules: создатели и `CreatorLevel`, запрет создателя в `power_levels.users`, `additional_creators`, `m.federate` по серверу создателя, create вне `auth_events` | `internal/federation/auth` (`Creators`, `CreatorsPrivileged`, `IsCreateHashRoomID`) |
| `createRoom` и `/upgrade` с версией 12: room_id из хеша create, create без room_id, `auth_events` без create, создатели вне users, tombstone 150, `trusted_private_chat` - приглашенные становятся `additional_creators`; совпавший room_id (тот же create в ту же миллисекунду) - сдвиг `origin_server_ts` на 1 мс | `internal/core/room.go`, `room_upgrade.go`; capabilities объявляют v12, по умолчанию по-прежнему v10 |
| импорт: фильтр `-server-name` для room_id без домена смотрит на создателя; комнаты считаются по версиям (`room_versions` в отчете, dry-run); версии без наших правил (`org.matrix.*`, 13+) - ошибка строки и код 1 до окна переключения; `event_id`, не совпавший с reference hash PDU, - ошибка и в `syngo-import` | `cmd/syngo-import` |
| федерация: `make_join` и прием приглашения в комнату v12 - `M_INCOMPATIBLE_ROOM_VERSION`, исходящий join v12 - отказ; затем - v12 по федерации в обе стороны (см. ниже) | `core.FederationMaxRoomVersion = 11`, затем 12 |

Проверка: фикстура живого Synapse 1.165 с комнатой v12 (дополнительный создатель, уровень
модератора, redaction создателем, алиас, приглашение) и комнатой v11, замененной на v12
(`tools/synapse-export/fixture/gen.sh`, `SCENARIO=v12` → `cmd/syngo-import/testdata/synapse-1.165-v12.sql`):
`TestImportSynapseRoomsV12` (export.sql, syngo-synapse-export, импорт, сверка `OK`, затем через HTTP
`/sync`, `/messages`, права создателей и новая комната v12), `TestCheckEventIDSynapseV12`
(reference hash всех событий v12 источника).

Не сделано на первом шаге (федерация тогда была вне скоупа): state resolution v2.1, прием PDU v12 по федерации,
выдача create v12 без `room_id`, уровни создателей в федеративном `canRedactOthers`.

Федерация v12 (state resolution v2.1):

| Что | Где |
|---|---|
| прием PDU v12 (`/send`, `/invite` v1/v2, `send_join`, `send_leave`): create без `room_id` (room_id выводится из его `event_id`), create с `room_id` и create в `auth_events` - 400; auth rules с неявным create, пустые `auth_events` у join создателя | `internal/api/federation_v12.go`, `checkAuthRules`, `auth.ValidateAuthEvents` |
| наша комната v12: `make_join`/`make_leave` - шаблон без create в `auth_events`; create - в auth-цепочке `send_join`, `/state_ids`, `/event_auth` | `templateAuthEvents`, `store.AuthChainIDs` |
| исходящие: `ver=12` в `make_join`, проверка ответа `send_join` (create без `room_id`, room_id = хеш create, create не в `auth_events`), `make_leave` и `make_join` через сервер создателя/пригласившего/участников и `via` (у room_id нет домена), `invite_room_state` из полных PDU (MSC4311) | `internal/federation/join`, `remoteRoomTarget`, `deliverRemoteInvite` |
| версия комнаты из `/v2/invite` сохраняется до join | `applyMembershipEvent` |
| создатели v12 удаляют чужие события по федерации | `store.canRedactOthers` |
