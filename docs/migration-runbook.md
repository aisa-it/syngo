# Migration runbook: переезд с Synapse на syngo

Процедура переключения и откат. Опирается на ADR-0006 (что переносим) и на команды
`syngo-import`, `syngo-import-events`, `syngo-import-media`, `syngo-export` (exit path).

> Пропускная способность - на синтетическом профиле приёмки: **100 пользователей, 200 комнат,
> 100 000 событий, 500 медиа** (PostgreSQL 17, локально, один процесс). Корректность и порядок
> шагов - на репетиции с живым Synapse 1.165 (`docs/migration-rehearsal.md`).
> Приемка: `./tools/migration-acceptance/run.sh "$TEST_DSN" synapse` (копия БД Synapse),
> `fixture` (фикстура живого Synapse), `full`/`small` (синтетика) - только на отдельной
> тестовой базе (имя содержит `test`): скрипт начинает с отката всех миграций и на другой
> базе отказывается работать. Боевой DSN сюда не передавать.

## 1. Что переносится и что нет

| Переносится | Команда |
|---|---|
| Пользователи (user_id, localpart, профиль из `profiles`, deactivated, admin, хеш пароля; locked - как deactivated) | `syngo-import` (kind=user) |
| Сессии: устройства (имя, last_seen), access и refresh токены (как SHA-256 - клиенты не перелогиниваются) | `syngo-import` (kind=device, access_token) |
| E2EE: ключи устройств, one-time и fallback keys, кросс-подписи и подписи, бэкап ключей (версии и ключи), недоставленные to-device | `syngo-import` (kind=device_keys, one_time_key, fallback_key, cross_signing_key, cross_signing_signature, room_keys_version, room_key, to_device) |
| Настройки клиента: account_data (в том числе секреты SSSS), комнатная account_data и теги, push rules, pushers, фильтры | `syngo-import` (kind=account_data, push_rule, pusher, filter) |
| Комнаты (версии 1-12, в том числе v12 с room_id без домена), алиасы | `syngo-import` (kind=room, room_alias) |
| История событий (`event_id`, `stream_ordering`, `depth`, `prev/auth_events`, `content`, исходный PDU с подписями и hashes, `event_auth`) | `syngo-synapse-export` → `syngo-import-events` |
| Отклоненные, soft-failed, outlier-события | `excluded_events`: сохранены, но не видны в `/messages`, `/sync`, поиске и не входят в состояние |
| Redactions | применяются при импорте: удаленные сообщения скрыты, оригинал - в `redactions` до `-redaction-retention` |
| Текущее состояние комнат | строки `kind=state` из `current_state_events` Synapse |
| Членство (материализация) | `syngo-import` (kind=membership, **после** событий: FK на `events`) |
| Отметки прочтения `m.read`, `m.read.private` локальных пользователей (`receipts_linearized`); `m.fully_read` - комнатная account_data | `syngo-import` (kind=receipt, с данными пользователей): /sync отдает их в `m.receipt`, непрочитанное после переезда считается от отметки |
| Медиа: метаданные + файлы, карантин, sha256 | `syngo-import-media` |
| Пароли | bcrypt-хеши переносятся как есть, **пере-хешируются в argon2id при первом входе**; `password_config.pepper` Synapse задать в `auth.password_pepper`, NFKC учитывается |
| Учетки LDAP (пустой хеш) | входят через `auth.ldap` (те же параметры, что у synapse-ldap3; ADR-0010) |
| SSO (`user_external_ids`) | `export.sql` + `syngo-import` (kind=user_external_id, сверка `external_ids`); вход - `auth.oidc_providers` с теми же `idp_id`, `server.public_baseurl` - тот же адрес callback, что у Synapse (ADR-0010) |

| Не переносится | Почему / что делать |
|---|---|
| presence, typing | эфемерные данные, восстановятся сами |
| receipts пользователей других серверов, `receipts_graph` | придут по федерации; `receipts_graph` дублирует `receipts_linearized` |
| suspended, shadow_banned | аналога в SYNGO нет: переносятся обычными пользователями, число - в отчете импорта |
| SAML, CAS, JWT-вход | не поддержаны (ADR-0010): перед переключением инсталляции с ними - блокер |
| Ключи федерации сервера | генерируются заново |
| `state_groups` | не нужны: текущее состояние берется из `current_state_events`; эвристика `syngo-recompute-state` - только для дампов старого формата |
| Превью ссылок (`url_cache`), миниатюры, `remote_media_cache` | кеш: превью и миниатюры syngod строит заново по запросу; чужие медиа - федерация вне скоупа |

## 2. Окно обслуживания

Решение по откату - ADR-0006 §7: односторонний переезд с окном приемки, без двойной записи.

1. **T-24ч**: предупредить пользователей, зафиксировать окно. Оценка простоя: на репетиции
   (`docs/migration-rehearsal.md`) переезд занял секунды; на боевом объеме время шагов 3.2-3.6
   масштабируется по замерам синтетики (~35 тыс. событий/с импорта) плюс копирование медиа -
   закладывать 2-4 ч на профиль до 1 млн событий и прогнать `run.sh synapse` на копии боевой БД заранее.
2. **T-24ч..T0**: медиа можно перенести заранее (шаг 3.6 на работающем Synapse): повторный запуск в окне
   докопирует только новое. Собрать команды заранее (`go build -o bin/ ./cmd/...`).
3. **T-1ч**: снизить TTL DNS до 60 с; сделать бэкап БД Synapse (`pg_dump -Fc`) - источник отката.
4. **T0**: **остановить Synapse** (все воркеры) - начало простоя. Не "read-only через блокировку POST на
   прокси": она ломает вход (`POST /login`), а работающий Synapse продолжает писать (federation, фоновые
   задачи) - снимок был бы несогласованным. Остановленный Synapse - это и есть точка отката без потерь.
5. **T0+**: импорт (шаг 3), проверки (шаг 4), бэкап БД SYNGO (`deploy/backup.sh`) до включения записи.
6. **Решение** (ответственный за миграцию): все проверки прошли - cutover (шаг 5); нет - откат (шаг 6.1).

## 3. Импорт (порядок обязателен)

```bash
set -euo pipefail
export PSQL_DSN='postgres://user:pass@host:5432/syngo?sslmode=disable'
export MEDIA_DIR=/var/lib/syngo/media          # наш ObjectStore
export SYNAPSE_MEDIA=/var/lib/matrix-synapse/media_store
export SYNAPSE_DSN='postgresql://synapse_ro@db/synapse'   # БД остановленного Synapse (только чтение)
export SERVER_NAME=matrix.example.org

# 3.0 (до окна) версии комнат источника: переносятся 1-12 (v12 - room_id без домена);
#     прочие (org.matrix.*, 13+) syngo-import не переносит - ошибка и код 1.
#     Тот же список печатает syngo-import -dry-run («версии комнат: ...»)
psql "$SYNAPSE_DSN" -At -c 'SELECT room_version, count(*) FROM rooms GROUP BY 1 ORDER BY 1'

# 3.1 схема
bin/migrate -dsn "$PSQL_DSN" -up

# 3.2 выгрузка Synapse. События - одним снимком по всем stream_ordering (включая
#     backfill < 0), с манифестом в конце; ошибка - ненулевой код.
psql "$SYNAPSE_DSN" -v ON_ERROR_STOP=1 -At -o all.jsonl -f tools/synapse-export/export.sql
bin/syngo-synapse-export -synapse-dsn "$SYNAPSE_DSN" -out events.jsonl

# 3.3 пользователи, комнаты, алиасы
grep -E '"kind":"(user|room|room_alias)"' all.jsonl > users-rooms.jsonl
bin/syngo-import -dsn "$PSQL_DSN" -file users-rooms.jsonl -server-name "$SERVER_NAME" -report r-users.json

# 3.3a данные пользователей: сессии, E2EE, бэкап ключей, настройки, отметки
#      прочтения (kind=receipt); затем сверка счетчиков по пользователям с
#      источником (user_stats), код 1 при расхождении. Позиции событий отметкам проставит
#      шаг 3.4 (receipts: positioned=N в отчете)
grep -vE '"kind":"(user|room|room_alias|membership|media)"' all.jsonl > userdata.jsonl
bin/syngo-import -dsn "$PSQL_DSN" -file userdata.jsonl -server-name "$SERVER_NAME" -report r-userdata.json -verify

# 3.4 история событий; в конце импорт пишет room_state из current_state_events, применяет
#     redaction, подтягивает sequence; дамп без манифеста или с меньшим числом событий не
#     принимается; после обрыва - тот же запуск: продолжит с чекпойнта
bin/syngo-import-events -dsn "$PSQL_DSN" -file events.jsonl -batch 1000 \
  -checkpoint events.checkpoint -bad-file events-bad.jsonl -report events-import.txt

# 3.4a только для баз из дампов старого формата (без kind=state) - пересчет состояния
#      эвристикой; для дампа syngo-synapse-export НЕ запускать: перезапишет
#      current_state_events Synapse
# bin/syngo-recompute-state -dsn "$PSQL_DSN" -batch 100 -report state-report.txt

# 3.5 членство — ПОСЛЕ событий (FK room_memberships.event_id → events)
grep -E '"kind":"membership"' all.jsonl > membership.jsonl
bin/syngo-import -dsn "$PSQL_DSN" -file membership.jsonl -server-name "$SERVER_NAME" -report r-members.json

# 3.6 медиа: метаданные + файлы. url_cache пропускается (статус url_cache),
#     карантин переносится, sha256 сверяется с Synapse; повторный запуск копирует только
#     недостающее (после обрыва или после предварительного переноса до окна)
grep -E '"kind":"media"' all.jsonl > media.jsonl
bin/syngo-import-media -dsn "$PSQL_DSN" -dump media.jsonl -media-store "$SYNAPSE_MEDIA" \
  -object-dir "$MEDIA_DIR" -server-name "$SERVER_NAME" -workers 8 -report media-import.txt

# 3.7 сверка с Synapse (не с дампом): события, stream_ordering, отклоненные вне timeline,
#     состояние = current_state_events, redaction, членство, пользователи, алиасы, медиа
#     (размер, карантин, sha256), sequence; расхождения - код 1 и отчет
bin/syngo-verify-migration -synapse-dsn "$SYNAPSE_DSN" -dsn "$PSQL_DSN" \
  -server-name "$SERVER_NAME" -report verify.txt
```

Отметки прочтения нужны до первого старта syngod: при старте он досчитывает непрочитанное
по истории (импорт больше 10 000 событий) или разбирает события по одному, и без отметок
непрочитанными стали бы все чужие сообщения после последнего своего. Если syngod
уже работал на перенесенной базе без отметок, их можно дозагрузить из того же `all.jsonl`:
импорт пересчитает уже посчитанные счетчики непрочитанного (`event_push_summary`) от отметок,
более поздние отметки, поставленные в SYNGO, не откатит. Затем перезапустить syngod: отметки
для `/sync` он читает из БД при старте.

```bash
grep -E '"kind":"receipt"' all.jsonl > receipts.jsonl
bin/syngo-import -dsn "$PSQL_DSN" -file receipts.jsonl -server-name "$SERVER_NAME" -report r-receipts.json
```

Все шаги, кроме выгрузки и сверки, - в `tools/migration-acceptance/run.sh` (профиль `synapse`), он же
проверяет вход, `/messages`, `/sync`, скачивание медиа и карантин через API поднятого syngod.

Замер приёмки (full-профиль синтетики): users+rooms **0.34 с**, события 100k **2.4–3.1 с** (~32–42k/с),
медиа 500 **0.75 с**, RSS процесса импорта событий **~24 MiB** (память не растёт с дампом).

## 4. Проверки после импорта (до включения записи)

| Проверка | Команда/ожидание |
|---|---|
| Коды возврата | все шаги 3.x завершились с кодом 0: `syngo-import`, `syngo-import-events`, `syngo-import-media` при любой ошибке строки возвращают ненулевой код (`bad`, `errors`, `problem:` в отчете) |
| Схема | `bin/migrate -dsn "$PSQL_DSN" -version` → версия, `dirty: false` |
| Сверка с Synapse | `syngo-verify-migration` (шаг 3.7) → `result: OK`: число событий и `min/max(stream_ordering)` равны источнику, события по комнатам и `event_id` совпадают, `class_diffs=0` (отклоненные не в timeline), `state: diffs=0`, `redactions: diffs=0`, `memberships: diffs=0`, `users`/`aliases`/`media` без расхождений, `last_value` sequence ≥ max |
| Медиа | отчет `media-import.txt`: `missing=0 mismatch=0 hash_mismatch=0 failed=0`, `url_cache` - число превью Synapse; повторный запуск - `copied=0` |
| API | `run.sh synapse` (или вручную): вход пользователя из Synapse → 200 (bcrypt пере-хешируется), `/messages` - те же `event_id`, `/sync` отдает состояние, `GET /_matrix/client/v1/media/download/<server>/<id>` - байты с sha256 из отчета, медиа в карантине - 404 |
| Бэкап | `deploy/backup.sh` БД SYNGO после проверок: точка повтора без повторного импорта |

## 5. Cutover

1. Переключить DNS/прокси `/.well-known/matrix/client` и `server_name` → новый сервер.
2. Включить запись на новом сервере (открыть прокси) - **точка невозврата без потерь** (ADR-0006 §7);
   записать время и `max(stream_ordering)` (`SELECT max(stream_ordering) FROM events`) - граница для отката.
3. Проверить: логин, отправка сообщения, получение через `/sync`, скачивание медиа, приглашение в комнату.
4. Synapse остается остановленным 24–72 ч, его БД и `media_store` не трогать - источник отката.

### Клиенты после переключения

Клиенты (Element, Element X, боты) остаются в сессии со своими токенами доступа и первым
`/sync` присылают `since` Synapse (`s72594_4483_...`, `m1234~1.1230~...`). Его позиция в нашу не
переводится, поэтому ответ - разрыв, а не новые события:

| Часть ответа | Что отдается |
|---|---|
| `rooms.join.*.timeline` | `events: []`, `limited: true`, `prev_batch` - позиция ответа; история - `GET /messages?dir=b&from=<prev_batch>` |
| `rooms.join.*.state` | текущее состояние комнаты целиком; с `lazy_load_members` - только свое членство (остальные - `summary` и `/members`) |
| `rooms.invite` | приглашения, как в initial sync |
| `rooms.leave` | выходы: state на момент выхода, timeline пуст, `limited: true` |
| account data, ephemeral, unread, to-device | как в initial sync |
| `next_batch` | токен syngod (`v1.e...`): следующий `/sync` - обычный incremental |

Клиент видит `limited: true` и считает это пропуском истории, а не новыми сообщениями: боты не отвечают
повторно на старые сообщения (раньше ответ был initial sync с последними сообщениями комнат в
виде incremental). В логе syngod такой запрос - `api: sync with Synapse since token` с `user_id` и
`device_id`, один раз на устройство. Sliding sync с `pos` Synapse получает `400 M_UNKNOWN_POS` и
начинает соединение заново.

## 6. Rollback

### 6.1 До включения записи в SYNGO (без потерь)

1. Остановить syngod, вернуть прокси/DNS на Synapse, запустить Synapse - он не менялся.
2. БД SYNGO выбросить (повтор миграции - с шага 3.1 на пустую базу).
3. Зафиксировать причину (что не сошлось), при необходимости - в ADR-0006.

### 6.2 После включения записи (с потерей созданного в SYNGO)

1. Остановить syngod (запись прекращается), бэкап БД SYNGO (`deploy/backup.sh`).
2. Выгрузить созданное после переключения - архив для ручного разбора:
   `syngo-export -dsn "$PSQL_DSN" -out after-cutover.jsonl -after-stream <max stream_ordering из 5.2>
   -media-since <время из 5.2> -media-dir "$MEDIA_DIR" -media-out after-cutover-media`.
   Загрузчика этого дампа в Synapse нет (ADR-0006 §7): события восстанавливаются вручную.
3. Вернуть прокси/DNS на Synapse, запустить Synapse.
4. **Пароли**: если пользователь сменил пароль в SYNGO, в Synapse действует старый; хеш argon2id в
   Synapse не переносится (нужен сброс пароля или SSO).
5. Предупредить пользователей о потере сообщений за период работы SYNGO; причину зафиксировать (при необходимости - в ADR-0006).

## 7. Чек-лист приемки

- [ ] Репетиция на копии боевого Synapse по `docs/migration-rehearsal.md`: `run.sh synapse` - `приемка пройдена`, протокол с временем шагов приложен.
- [ ] `syngo-verify-migration` против Synapse - `result: OK` (события, состояние, redaction, членство, пользователи, алиасы, медиа).
- [ ] Медиа: `missing=0 mismatch=0 hash_mismatch=0 failed=0`, карантин сохранен (404), `url_cache` пропущен, повторный запуск - `copied=0`.
- [ ] Логин пользователя из дампа работает, пароль мигрировал в argon2id.
- [ ] `/messages` отдаёт исходные `event_id` в исходном порядке.
- [ ] `events_stream_ordering_seq` ≥ `max(stream_ordering)`.
- [ ] Exit path (`syngo-export`) проверен round-trip-тестом `TestRoundTripSynapseFixture`.
- [ ] Откат 6.1 отрепетирован (Synapse после запуска принимает вход и запись).
- [ ] Найденные расхождения заведены задачами.

## 8. Известные ограничения, найденные при приёмке

1. ~~`room_state` не материализуется импортом~~ - **исправлено:** дамп
   `syngo-synapse-export` несет `current_state_events` (строки `kind=state`), импорт пишет их в `room_state`
   как есть; эвристика (последнее state-событие по `(type,state_key)`, `syngo-recompute-state`) - только для
   дампов старого формата. Сверка состояния с Synapse - `syngo-verify-migration` после шага 3.5.
2. **В этом билде нет `GET /rooms/{roomId}/state/{type}/{key}` и `/joined_rooms`** — приёмочные
   проверки состояния приходится делать через БД; эндпоинты из api-scope P0 стоит доделать.
3. ~~`syngo-import` импортирует события по одному~~ — **исправлено:** `syngo-import`
   читает дамп потоком и пишет события тем же импортером пачками (с подтягиванием sequence); для
   больших дампов по-прежнему удобнее `syngo-import-events` (чекпойнт, карантин строк).
4. **Percent-encoded `room_id` в пути не находится** (chi не декодирует параметр) — клиенты и
   тесты должны слать `!room:server` как есть.
5. **Комнаты v12 переносятся и работают для клиентов, но не по федерации:** `make_join` и
   приглашения в комнату v12 с другого сервера отклоняются (`M_INCOMPATIBLE_ROOM_VERSION`). Комнаты
   версий без наших правил (`org.matrix.*`, 13+) не переносятся: `syngo-import` завершается с ошибкой
   до окна переключения (ADR-0006 §11).
