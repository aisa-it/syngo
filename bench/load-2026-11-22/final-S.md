# Нагрузочный прогон: профиль S против syngo

Сгенерировано `cmd/loadgen`, 2026-11-22T03:56:58Z. Прогон `2ead82e5`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 20.4, go1.27.0, PostgreSQL 17.12 |
| db_max_conns | 20 |
| log_level | info |
| rate_limit | off |
| server_commit | - |
| server_name | bench.loadtest |

## Профиль

| Параметр | Значение |
|---|---|
| Пользователи / комнаты / комнат на пользователя | 50 / 20 / 5 (Ципф 1.0, шифрованных 30%) |
| Модель | закрытая, think time 5s (≈10 действий/с) |
| Ramp / warmup / замер / drain | 15s / 20s / 3m0s / 10s |
| /sync | timeout 30s, timeline 20, lazy_load_members true |
| Веса действий | send 40, typing 15, receipt 15, messages 6, keys_query 6, media_download 6, media_upload 3, join_leave 3, invite 3, keys_upload 2, create_room 1 |

## Подготовка

7.0 с: пользователей 50 (зарегистрировано 50, вход 0, из кеша 0), комнат создано 20, переиспользовано 0, входов 250, членств 250 (самая большая комната 40, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 180.2 с |
| Действий | 1789 (9.9/с, профиль 10.0/с) |
| Запросов | 56403 (313.0/с) |
| Ошибок (не 2xx и сеть) | 0, из них 429: 0; доля без 429: 0.0000 |
| Комнат / членств к концу | 36 / 317 |
| sync_limited_timelines | 42 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 188 | 328 | 405 | 613 | 12099 |
| от начала PUT, мс | 325 | 524 | 633 | 935 | 12099 |

Сообщений 691, ожидалось доставок 12099, доставлено 12099 (полнота 1.000), раньше ответа отправителю 0, сообщений с недоставкой 0.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 54573 | 302.89 | 135 | 366 | 602 | 1181 | 0 | 0 | 200:54573 |
| send_text | 382 | 2.12 | 141 | 239 | 374 | 592 | 0 | 0 | 200:382 |
| send_encrypted | 308 | 1.71 | 123 | 244 | 359 | 526 | 0 | 0 | 200:308 |
| receipt | 277 | 1.54 | 71.6 | 223 | 540 | 975 | 0 | 0 | 200:277 |
| typing | 276 | 1.53 | 25.2 | 47.6 | 62.3 | 67.9 | 0 | 0 | 200:276 |
| messages | 125 | 0.69 | 70.2 | 134 | 154 | 167 | 0 | 0 | 200:125 |
| media_download | 109 | 0.60 | 20.6 | 38.6 | 45.3 | 48.9 | 0 | 0 | 200:109 |
| keys_query | 108 | 0.60 | 39.4 | 74.5 | 84.8 | 91.5 | 0 | 0 | 200:108 |
| join_invite | 58 | 0.32 | 196 | 348 | 562 | 563 | 0 | 0 | 200:58 |
| media_upload | 54 | 0.30 | 39.0 | 162 | 621 | 621 | 0 | 0 | 200:54 |
| keys_upload | 36 | 0.20 | 45.8 | 100 | 312 | 313 | 0 | 0 | 200:36 |
| leave | 34 | 0.19 | 125 | 241 | 263 | 263 | 0 | 0 | 200:34 |
| invite | 27 | 0.15 | 134 | 234 | 283 | 283 | 0 | 0 | 200:27 |
| join | 21 | 0.12 | 177 | 264 | 282 | 282 | 0 | 0 | 200:21 |
| create_room | 15 | 0.08 | 456 | 757 | 757 | 761 | 0 | 0 | 200:15 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 98.4 / 242 / 396 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 561824 / 3466.280 / 20 |
| GC: циклов / паузы, с / куча в конце, МиБ | 614 / 1.336 / 11.9 |
| Макс. long-poll / горутин | 50 / 126 |
| Коды ответов | 200:56398 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 54568 | 437 | 752 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 692 | 247 | 449 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 277 | 238 | 538 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 275 | 49.0 | 84.7 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 125 | 219 | 244 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 109 | 46.2 | 49.2 |
| POST /_matrix/client/v3/keys/query | 107 | 91.1 | 98.2 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 58 | 480 | 855 |
| POST /_matrix/media/v3/upload | 54 | 145 | 730 |
| POST /_matrix/client/v3/keys/upload | 36 | 97.1 | 410 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 34 | 245 | 415 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 27 | 247 | 432 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 21 | 412 | 482 |
| POST /_matrix/client/v3/createRoom | 15 | 906 | 981 |

## Процесс сервера

PID 1723252: RSS 58036 -> 61148 кБ (макс. 61080), CPU 198.8 с за окно (в среднем 110%, пик 133% одного ядра), потоков до 17.

## PostgreSQL

PostgreSQL 17.12. TPS 3422.5, cache hit 1.000, соединений базы до 22 (активных до 16), размер 13.6 -> 20.7 МиБ.

pg_stat_database за окно: xact_commit 616441, xact_rollback 0, blks_read 0, blks_hit 6932368, tup_returned 8761585, tup_fetched 2750727, tup_inserted 17882, tup_updated 14930, tup_deleted 9569, temp_files 0, temp_bytes 0, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `SELECT greatest( coalesce((SELECT max(stream_ordering) FROM events), $1), coalesce((SELECT max(stream_ordering) FROM dev…` | 79798 | 51197 | 0.64 |
| `WITH changed AS MATERIALIZED ( SELECT DISTINCT user_id FROM device_lists_changes WHERE stream_ordering > $2 AND stream_o…` | 33299 | 30381 | 0.91 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 54631 | 29119 | 0.53 |
| `SELECT rm.room_id, rm.membership, coalesce((SELECT e.stream_ordering FROM events e WHERE e.event_id = rm.event_id), $2),…` | 54537 | 24643 | 0.45 |
| `SELECT DISTINCT ON (e.room_id, e.type, e.state_key) e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.p…` | 13458 | 22789 | 1.69 |
| `SELECT u.user_id, u.localpart, u.display_name, t.device_id, u.deactivated, t.expires_at, t.scope, t.last_validated_at FR…` | 56365 | 16014 | 0.28 |
| `SELECT algorithm, count(*) FROM one_time_keys WHERE user_id = $1 AND device_id = $2 AND claimed_at IS NULL GROUP BY algo…` | 54601 | 13431 | 0.25 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 15217 | 13167 | 0.86 |
| `WITH ins AS ( INSERT INTO event_push_actions (user_id, room_id, event_id, stream_ordering, thread_id, highlight) SELECT …` | 588 | 8345 | 14.19 |
| `SELECT id, event_type, sender, content, stream_ordering FROM device_inbox WHERE user_id = $1 AND device_id = $2 AND stre…` | 54539 | 8173 | 0.15 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0 | <= 0.01 | ok |
| delivery.completeness | гейт | 1 | >= 0.99 | ok |
| delivery.messages | гейт | 691 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 328.276 | <= 1000 | ok |
| delivery.from_ack.p99_ms | SLO (предложение) | 404.565 | <= 2000 | ok |
| send.p95_ms | SLO (предложение) | 238.756 | <= 500 | ok |
