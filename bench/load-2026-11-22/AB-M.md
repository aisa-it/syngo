# Нагрузочный прогон: профиль M против syngo

Сгенерировано `cmd/loadgen`, 2026-11-21T20:47:17Z. Прогон `2658b2ae`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 19.6, go1.27.0, PostgreSQL 17.12 |
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

92.3 с: пользователей 500 (зарегистрировано 500, вход 0, из кеша 0), комнат создано 200, переиспользовано 0, входов 5000, членств 5000 (самая большая комната 449, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 301.4 с |
| Действий | 15754 (52.3/с, профиль 100.0/с) |
| Запросов | 33679 (111.7/с) |
| Ошибок (не 2xx и сеть) | 17, из них 429: 0; доля без 429: 0.0005 |
| Комнат / членств к концу | 372 / 5788 |
| sync_limited_timelines | 349 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 10473 | 15286 | 17224 | 203393 | 628565 |
| от начала PUT, мс | 17571 | 22759 | 24891 | 210679 | 628565 |

Сообщений 6105, ожидалось доставок 632739, доставлено 628565 (полнота 0.993), раньше ответа отправителю 0, сообщений с недоставкой 952.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 17350 | 57.57 | 8331 | 10684 | 11919 | 14601 | 0 | 0 | 200:17350 |
| send_text | 3916 | 12.99 | 6827 | 8583 | 9576 | 10556 | 8 | 0 | 200:3908 403:8 |
| receipt | 2385 | 7.91 | 2474 | 3721 | 4363 | 6034 | 1 | 0 | 200:2384 403:1 |
| typing | 2342 | 7.77 | 1432 | 2308 | 3111 | 3422 | 5 | 0 | 200:2337 403:5 |
| send_encrypted | 2292 | 7.61 | 6827 | 8583 | 9576 | 10493 | 2 | 0 | 200:2290 403:2 |
| media_download | 933 | 3.10 | 952 | 1729 | 2474 | 2613 | 0 | 0 | 200:933 |
| messages | 933 | 3.10 | 4193 | 5765 | 6305 | 6720 | 0 | 0 | 200:933 |
| keys_query | 931 | 3.09 | 1929 | 2901 | 3612 | 3864 | 0 | 0 | 200:931 |
| join_invite | 761 | 2.52 | 8086 | 10064 | 11569 | 11949 | 0 | 0 | 200:761 |
| media_upload | 471 | 1.56 | 981 | 1662 | 2354 | 2859 | 0 | 0 | 200:471 |
| invite | 388 | 1.29 | 8843 | 10898 | 12159 | 12444 | 1 | 0 | 200:387 403:1 |
| leave | 348 | 1.16 | 6432 | 8248 | 9021 | 9906 | 0 | 0 | 200:348 |
| keys_upload | 332 | 1.10 | 1490 | 2524 | 3173 | 3575 | 0 | 0 | 200:332 |
| join | 154 | 0.51 | 8086 | 9768 | 11229 | 11858 | 0 | 0 | 200:154 |
| create_room | 143 | 0.47 | 21654 | 25901 | 26422 | 26709 | 0 | 0 | 200:143 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 7654 / 12806 / 26561 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 445885 / 223298.441 / 10 |
| GC: циклов / паузы, с / куча в конце, МиБ | 184 / 0.588 / 193.5 |
| Макс. long-poll / горутин | 0 / 1634 |
| Коды ответов | 200:33681 403:18 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 17349 | 21135 | 28227 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 6223 | 9778 | 9980 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 2389 | 4735 | 4951 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 2342 | 2472 | 4219 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 933 | 8445 | 9689 |
| POST /_matrix/client/v3/keys/query | 932 | 3911 | 4782 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 932 | 2330 | 2490 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 760 | 14490 | 26898 |
| POST /_matrix/media/v3/upload | 472 | 2358 | 2488 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 392 | 24400 | 28880 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 348 | 9750 | 9950 |
| POST /_matrix/client/v3/keys/upload | 332 | 2559 | 4512 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 152 | 9813 | 14800 |
| POST /_matrix/client/v3/createRoom | 143 | 29000 | 29800 |

## Процесс сервера

PID 1248554: RSS 259268 -> 338896 кБ (макс. 363976), CPU 323.1 с за окно (в среднем 107%, пик 159% одного ядра), потоков до 16.

## PostgreSQL

PostgreSQL 17.12. TPS 1477.2, cache hit 1.000, соединений базы до 12 (активных до 10), размер 71.2 -> 146.7 МиБ.

pg_stat_database за окно: xact_commit 446213, xact_rollback 0, blks_read 817, blks_hit 63359626, tup_returned 72726118, tup_fetched 62126285, tup_inserted 246674, tup_updated 10755, tup_deleted 116900, temp_files 0, temp_bytes 0, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `SELECT room_id, count(*) FILTER (WHERE membership = $3), count(*) FILTER (WHERE membership = $4), coalesce((array_agg(us…` | 17337 | 190533 | 10.99 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 17343 | 160195 | 9.24 |
| `WITH changed AS MATERIALIZED ( SELECT DISTINCT user_id FROM device_lists_changes WHERE stream_ordering > $2 AND stream_o…` | 17328 | 86291 | 4.98 |
| `SELECT e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.prev_events, e.auth_events, e.depth, e.origin_…` | 18322 | 43300 | 2.36 |
| `SELECT room_id, thread_id, count(*), count(*) FILTER (WHERE highlight) FROM event_push_actions WHERE user_id = $1 AND ro…` | 17362 | 31072 | 1.79 |
| `SELECT DISTINCT ON (e.room_id, e.type, e.state_key) e.event_id, e.room_id, e.type, e.sender, e.state_key, e.content, e.p…` | 4973 | 26169 | 5.26 |
| `WITH ins AS ( INSERT INTO event_push_actions (user_id, room_id, event_id, stream_ordering, thread_id, highlight) SELECT …` | 7 | 23374 | 3339.09 |
| `SELECT DISTINCT substr(rs.state_key, strpos(rs.state_key, $2) + $3) FROM room_state rs JOIN events e ON e.event_id = rs.…` | 7801 | 19322 | 2.48 |
| `SELECT pg_advisory_xact_lock(hashtext($2 \|\| $1))` | 15603 | 13841 | 0.89 |
| `SELECT greatest( coalesce((SELECT max(stream_ordering) FROM events), $1), coalesce((SELECT max(stream_ordering) FROM dev…` | 25345 | 11929 | 0.47 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0.001 | <= 0.01 | ok |
| delivery.completeness | гейт | 0.993 | >= 0.99 | ok |
| delivery.messages | гейт | 6105 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 15285.839 | <= 1000 | НЕ ПРОЙДЕНА |
| delivery.from_ack.p99_ms | SLO (предложение) | 17224.466 | <= 2000 | НЕ ПРОЙДЕНА |
| send.p95_ms | SLO (предложение) | 8583.207 | <= 500 | НЕ ПРОЙДЕНА |
