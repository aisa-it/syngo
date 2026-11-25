# Нагрузочный прогон: профиль M против syngo

Сгенерировано `cmd/loadgen`, 2026-11-21T19:12:26Z. Прогон `8f79a01f`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 3.3, go1.27.0, PostgreSQL 17.12 |
| db_max_conns | 10 |
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

199.8 с: пользователей 500 (зарегистрировано 450, вход 0, из кеша 50), комнат создано 180, переиспользовано 20, входов 5000, членств 5000 (самая большая комната 449, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 300.1 с |
| Действий | 6526 (21.7/с, профиль 100.0/с) |
| Запросов | 9584 (31.9/с) |
| Ошибок (не 2xx и сеть) | 1247, из них 429: 0; доля без 429: 0.1301 |
| Комнат / членств к концу | 286 / 5528 |
| sync_limited_timelines | 3444 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 31292 | 54088 | 64698 | 90339 | 90277 |
| от начала PUT, мс | 50954 | 69365 | 77388 | 109089 | 90277 |

Сообщений 2310, ожидалось доставок 279151, доставлено 90277 (полнота 0.323), раньше ответа отправителю 52, сообщений с недоставкой 1695.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 3070 | 10.23 | 47525 | 59747 | 59747 | 60014 | 1193 | 0 | 200:1877 net:1193 |
| send_text | 1512 | 5.04 | 23919 | 53023 | 59747 | 60002 | 19 | 0 | 200:1493 403:2 net:17 |
| send_encrypted | 1047 | 3.49 | 22533 | 51978 | 59747 | 60001 | 13 | 0 | 200:1034 net:13 |
| typing | 943 | 3.14 | 4.2 | 328 | 804 | 1376 | 2 | 0 | 200:941 403:2 |
| receipt | 884 | 2.95 | 23448 | 51978 | 59747 | 60004 | 14 | 0 | 200:870 403:1 net:13 |
| messages | 397 | 1.32 | 24.0 | 692 | 1173 | 1717 | 0 | 0 | 200:397 |
| keys_query | 396 | 1.32 | 6.4 | 312 | 633 | 835 | 0 | 0 | 200:396 |
| media_download | 395 | 1.32 | 41.0 | 679 | 1417 | 1831 | 0 | 0 | 200:395 |
| media_upload | 193 | 0.64 | 45.3 | 499 | 1729 | 1768 | 0 | 0 | 200:193 |
| join_invite | 177 | 0.59 | 20603 | 35260 | 38795 | 38795 | 0 | 0 | 200:177 |
| invite | 164 | 0.55 | 22310 | 52498 | 59747 | 60007 | 3 | 0 | 200:161 net:3 |
| keys_upload | 133 | 0.44 | 66.1 | 627 | 1031 | 1177 | 0 | 0 | 200:133 |
| join | 112 | 0.37 | 21017 | 43024 | 52498 | 58510 | 0 | 0 | 200:112 |
| leave | 108 | 0.36 | 29186 | 57416 | 59747 | 60001 | 3 | 0 | 200:105 net:3 |
| create_room | 53 | 0.18 | 23683 | 39732 | 58570 | 58702 | 0 | 0 | 200:53 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 30000 / 30000 / 30000 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 55821 / 1894.898 / 10 |
| GC: циклов / паузы, с / куча в конце, МиБ | 106 / 0.145 / 413.9 |
| Макс. long-poll / горутин | 0 / 2329 |
| Коды ответов | 200:9145 403:5 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 2642 | 30000 | 30000 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 2554 | 30000 | 30000 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 943 | 364 | 878 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 884 | 30000 | 30000 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 397 | 755 | 2004 |
| POST /_matrix/client/v3/keys/query | 396 | 326 | 752 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 395 | 729 | 1908 |
| POST /_matrix/media/v3/upload | 193 | 490 | 2018 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 177 | 30000 | 30000 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 164 | 30000 | 30000 |
| POST /_matrix/client/v3/keys/upload | 133 | 768 | 1502 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 112 | 30000 | 30000 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 107 | 30000 | 30000 |
| POST /_matrix/client/v3/createRoom | 53 | 30000 | 30000 |

## Процесс сервера

PID 1109943: RSS 282828 -> 625888 кБ (макс. 608292), CPU 156.2 с за окно (в среднем 52%, пик 103% одного ядра), потоков до 17.

## PostgreSQL

PostgreSQL 17.12. TPS 426.3, cache hit 0.999, соединений базы до 12 (активных до 10), размер 95.5 -> 199.4 МиБ.

pg_stat_database за окно: xact_commit 127978, xact_rollback 0, blks_read 23154, blks_hit 27683402, tup_returned 68642526, tup_fetched 22301825, tup_inserted 328183, tup_updated 8300, tup_deleted 135883, temp_files 0, temp_bytes 0, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 3069 | 75926 | 24.74 |
| `WITH ins AS ( INSERT INTO event_push_actions (user_id, room_id, event_id, stream_ordering, thread_id, highlight) SELECT …` | 2062 | 50384 | 24.43 |
| `WITH changed AS MATERIALIZED ( SELECT DISTINCT user_id FROM device_lists_changes WHERE stream_ordering > $2 AND stream_o…` | 3069 | 32952 | 10.74 |
| `SELECT room_id, count(*) FILTER (WHERE membership = $3), count(*) FILTER (WHERE membership = $4), coalesce((array_agg(us…` | 3056 | 30683 | 10.04 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 3661 | 12420 | 3.39 |
| `SELECT pg_advisory_xact_lock(hashtext('room_state:' \|\| $1))` | 6327 | 9568 | 1.51 |
| `INSERT INTO events (event_id, room_id, type, sender, state_key, content, prev_events, auth_events, depth, origin_server_…` | 3641 | 8163 | 2.24 |
| `SELECT rs.room_id, rs.type, rs.state_key, e.sender, e.content FROM room_state rs JOIN events e ON e.event_id = rs.event_…` | 2063 | 6381 | 3.09 |
| `SELECT DISTINCT substr(rs.state_key, strpos(rs.state_key, $2) + $3) FROM room_state rs JOIN events e ON e.event_id = rs.…` | 3164 | 5879 | 1.86 |
| `SELECT m.room_id, m.user_id, coalesce((SELECT j.stream_ordering FROM events j WHERE j.event_id = m.event_id), $2) FROM r…` | 2063 | 5525 | 2.68 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0.13 | <= 0.01 | НЕ ПРОЙДЕНА |
| delivery.completeness | гейт | 0.323 | >= 0.99 | НЕ ПРОЙДЕНА |
| delivery.messages | гейт | 2310 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 54088.353 | <= 1000 | НЕ ПРОЙДЕНА |
| delivery.from_ack.p99_ms | SLO (предложение) | 64697.647 | <= 2000 | НЕ ПРОЙДЕНА |
| send.p95_ms | SLO (предложение) | 53022.599 | <= 500 | НЕ ПРОЙДЕНА |
