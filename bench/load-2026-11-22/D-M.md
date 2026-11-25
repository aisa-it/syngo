# Нагрузочный прогон: профиль M против syngo

Сгенерировано `cmd/loadgen`, 2026-11-22T03:51:58Z. Прогон `01cbf679`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 22.4, go1.27.0, PostgreSQL 17.12 |
| db_max_conns | 20 |
| log_level | info |
| rate_limit | off |
| server_commit | - |
| server_name | bench.loadtest |

## Профиль

| Параметр | Значение |
|---|---|
| Пользователи / комнаты / комнат на пользователя | 500 / 200 / 10 (Ципф 1.0, шифрованных 30%) |
| Модель | закрытая, think time 5s (≈100 действий/с) |
| Ramp / warmup / замер / drain | 1m0s / 1m30s / 5m0s / 15s |
| /sync | timeout 30s, timeline 20, lazy_load_members true |
| Веса действий | send 40, typing 15, receipt 15, messages 6, keys_query 6, media_download 6, media_upload 3, join_leave 3, invite 3, keys_upload 2, create_room 1 |

## Подготовка

62.0 с: пользователей 500 (зарегистрировано 500, вход 0, из кеша 0), комнат создано 200, переиспользовано 0, входов 5000, членств 5000 (самая большая комната 449, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 300.8 с |
| Действий | 21235 (70.6/с, профиль 100.0/с) |
| Запросов | 60986 (202.7/с) |
| Ошибок (не 2xx и сеть) | 17, из них 429: 0; доля без 429: 0.0003 |
| Комнат / членств к концу | 449 / 6047 |
| sync_limited_timelines | 447 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 4632 | 7542 | 8932 | 161511 | 885129 |
| от начала PUT, мс | 7926 | 11569 | 13036 | 165075 | 885129 |

Сообщений 8377, ожидалось доставок 886040, доставлено 885129 (полнота 0.999), раньше ответа отправителю 0, сообщений с недоставкой 744.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 39004 | 129.65 | 3721 | 5324 | 6432 | 7698 | 0 | 0 | 200:39004 |
| send_text | 5336 | 17.74 | 3049 | 4632 | 5485 | 6296 | 12 | 0 | 200:5324 403:12 |
| receipt | 3261 | 10.84 | 1094 | 1968 | 2788 | 3338 | 2 | 0 | 200:3259 403:2 |
| typing | 3149 | 10.47 | 633 | 1197 | 1747 | 2285 | 1 | 0 | 200:3148 403:1 |
| send_encrypted | 3114 | 10.35 | 3111 | 4868 | 5595 | 6532 | 2 | 0 | 200:3112 403:2 |
| keys_query | 1271 | 4.22 | 845 | 1403 | 2008 | 2594 | 0 | 0 | 200:1271 |
| media_download | 1266 | 4.21 | 421 | 836 | 1296 | 1960 | 0 | 0 | 200:1266 |
| messages | 1216 | 4.04 | 1800 | 2761 | 3576 | 4089 | 0 | 0 | 200:1216 |
| join_invite | 985 | 3.27 | 3612 | 5485 | 6432 | 6628 | 0 | 0 | 200:985 |
| media_upload | 613 | 2.04 | 442 | 888 | 1417 | 2104 | 0 | 0 | 200:613 |
| invite | 502 | 1.67 | 3911 | 5765 | 6561 | 7031 | 0 | 0 | 200:502 |
| leave | 492 | 1.64 | 2901 | 4632 | 5431 | 5684 | 0 | 0 | 200:492 |
| keys_upload | 413 | 1.37 | 672 | 1283 | 2008 | 2524 | 0 | 0 | 200:413 |
| create_room | 207 | 0.69 | 9866 | 13298 | 14257 | 14291 | 0 | 0 | 200:207 |
| join | 157 | 0.52 | 3612 | 5065 | 5881 | 6018 | 0 | 0 | 200:157 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 3807 / 5391 / 9078 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 848179 / 184964.316 / 20 |
| GC: циклов / паузы, с / куча в конце, МиБ | 471 / 1.475 / 128.5 |
| Макс. long-poll / горутин | 0 / 1502 |
| Коды ответов | 200:60959 403:17 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 38987 | 6862 | 9372 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 8447 | 4963 | 8633 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 3263 | 2435 | 3725 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 3155 | 1567 | 2318 |
| POST /_matrix/client/v3/keys/query | 1273 | 2138 | 2458 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 1265 | 938 | 2001 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 1220 | 3360 | 4672 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 981 | 6815 | 9363 |
| POST /_matrix/media/v3/upload | 614 | 982 | 2132 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 501 | 7803 | 9561 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 494 | 4964 | 8700 |
| POST /_matrix/client/v3/keys/upload | 414 | 1762 | 2452 |
| POST /_matrix/client/v3/createRoom | 207 | 27798 | 29560 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 155 | 5694 | 9139 |

## Процесс сервера

PID 1717270: RSS 219584 -> 281156 кБ (макс. 301852), CPU 402.2 с за окно (в среднем 133%, пик 168% одного ядра), потоков до 17.

## PostgreSQL

PostgreSQL 17.12. TPS 2811.8, cache hit 1.000, соединений базы до 22 (активных до 18), размер 88.2 -> 199.7 МиБ.

pg_stat_database за окно: xact_commit 848080, xact_rollback 0, blks_read 15109, blks_hit 89953864, tup_returned 83790781, tup_fetched 73878125, tup_inserted 313329, tup_updated 47890, tup_deleted 90062, temp_files 4, temp_bytes 10048896, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 38971 | 290750 | 7.46 |
| `SELECT room_id, count(*) FILTER (WHERE membership = $3), count(*) FILTER (WHERE membership = $4), coalesce((array_agg(us…` | 36432 | 189598 | 5.20 |
| `WITH changed AS MATERIALIZED ( SELECT DISTINCT user_id FROM device_lists_changes WHERE stream_ordering > $2 AND stream_o…` | 38947 | 174537 | 4.48 |
| `SELECT DISTINCT ON (e.room_id, e.type, e.state_key) e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.p…` | 15141 | 86804 | 5.73 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 40256 | 70213 | 1.74 |
| `WITH ins AS ( INSERT INTO event_push_actions (user_id, room_id, event_id, stream_ordering, thread_id, highlight) SELECT …` | 9 | 42931 | 4770.07 |
| `SELECT greatest( coalesce((SELECT max(stream_ordering) FROM events), $1), coalesce((SELECT max(stream_ordering) FROM dev…` | 49719 | 27525 | 0.55 |
| `SELECT rm.room_id, rm.membership, coalesce((SELECT e.stream_ordering FROM events e WHERE e.event_id = rm.event_id), $2),…` | 38967 | 21594 | 0.55 |
| `WITH room_group AS ( INSERT INTO room_state_groups (room_id, state_group) VALUES ($1, $3) ON CONFLICT (room_id) DO UPDAT…` | 12354 | 21241 | 1.72 |
| `INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events, auth_events, depth, origin_server_…` | 12354 | 20054 | 1.62 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0 | <= 0.01 | ok |
| delivery.completeness | гейт | 0.999 | >= 0.99 | ok |
| delivery.messages | гейт | 8377 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 7541.743 | <= 1000 | НЕ ПРОЙДЕНА |
| delivery.from_ack.p99_ms | SLO (предложение) | 8931.72 | <= 2000 | НЕ ПРОЙДЕНА |
| send.p95_ms | SLO (предложение) | 4631.53 | <= 500 | НЕ ПРОЙДЕНА |
