# Нагрузочный прогон: профиль S против syngo

Сгенерировано `cmd/loadgen`, 2026-11-21T18:04:49Z. Прогон `72deecfd`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.4 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 11.1, go1.27.4, PostgreSQL 17.12 |
| db_max_conns | 30 |
| log_level | info |
| rate_limit | prod |
| server_commit | - |
| server_name | bench.loadtest |

## Профиль

| Параметр | Значение |
|---|---|
| Пользователи / комнаты / комнат на пользователя | 50 / 20 / 5 (Ципф 1.0, шифрованных 30%) |
| Модель | закрытая, think time 5s (≈10 действий/с) |
| Ramp / warmup / замер / drain | 15s / 20s / 1m0s / 10s |
| /sync | timeout 30s, timeline 20, lazy_load_members true |
| Веса действий | send 40, typing 15, receipt 15, messages 6, keys_query 6, media_download 6, media_upload 3, join_leave 3, invite 3, keys_upload 2, create_room 1 |

## Подготовка

2.5 с: пользователей 50 (зарегистрировано 0, вход 0, из кеша 50), комнат создано 0, переиспользовано 20, входов 250, членств 250 (самая большая комната 40, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 60.1 с |
| Действий | 531 (8.8/с, профиль 10.0/с) |
| Запросов | 6593 (109.7/с) |
| Ошибок (не 2xx и сеть) | 0, из них 429: 0; доля без 429: 0.0000 |
| Комнат / членств к концу | 29 / 302 |
| sync_limited_timelines | 13 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 225 | 652 | 879 | 1455 | 3813 |
| от начала PUT, мс | 474 | 1505 | 2308 | 2628 | 3813 |

Сообщений 194, ожидалось доставок 3813, доставлено 3813 (полнота 1.000), раньше ответа отправителю 0, сообщений с недоставкой 0.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 6047 | 100.64 | 494 | 991 | 1712 | 2607 | 0 | 0 | 200:6047 |
| send_text | 101 | 1.68 | 208 | 870 | 1258 | 1259 | 0 | 0 | 200:101 |
| send_encrypted | 95 | 1.58 | 229 | 1197 | 2079 | 2079 | 0 | 0 | 200:95 |
| typing | 93 | 1.55 | 4.5 | 261 | 748 | 748 | 0 | 0 | 200:93 |
| receipt | 80 | 1.33 | 120 | 796 | 1580 | 1580 | 0 | 0 | 200:80 |
| media_download | 36 | 0.60 | 64.8 | 397 | 540 | 541 | 0 | 0 | 200:36 |
| messages | 35 | 0.58 | 24.2 | 218 | 266 | 267 | 0 | 0 | 200:35 |
| keys_query | 34 | 0.57 | 18.1 | 303 | 420 | 420 | 0 | 0 | 200:34 |
| join_invite | 19 | 0.32 | 239 | 1962 | 1962 | 1962 | 0 | 0 | 200:19 |
| keys_upload | 15 | 0.25 | 50.1 | 842 | 842 | 842 | 0 | 0 | 200:15 |
| media_upload | 14 | 0.23 | 78.3 | 1531 | 1531 | 1531 | 0 | 0 | 200:14 |
| invite | 9 | 0.15 | 148 | 315 | 315 | 315 | 0 | 0 | 200:9 |
| leave | 7 | 0.12 | 342 | 1658 | 1658 | 1658 | 0 | 0 | 200:7 |
| create_room | 4 | 0.07 | 456 | 1322 | 1322 | 1325 | 0 | 0 | 200:4 |
| join | 4 | 0.07 | 186 | 290 | 290 | 290 | 0 | 0 | 200:4 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 44.4 / 528 / 965 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 11799 / 50.200 / 30 |
| GC: циклов / паузы, с / куча в конце, МиБ | 65 / 0.069 / 13.3 |
| Макс. long-poll / горутин | 32 / 130 |
| Коды ответов | 200:6593 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 6047 | 994 | 2253 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 196 | 1275 | 2255 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 93 | 294 | 845 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 80 | 917 | 2100 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 36 | 450 | 820 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 35 | 222 | 412 |
| POST /_matrix/client/v3/keys/query | 34 | 358 | 472 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 19 | 1075 | 2215 |
| POST /_matrix/client/v3/keys/upload | 15 | 625 | 925 |
| POST /_matrix/media/v3/upload | 14 | 1450 | 2290 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 9 | 388 | 478 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 7 | 2238 | 2448 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 4 | 450 | 490 |
| POST /_matrix/client/v3/createRoom | 4 | 2350 | 2470 |

## Процесс сервера

PID 1050648: RSS 45936 -> 48176 кБ (макс. 49884), CPU 31.0 с за окно (в среднем 52%, пик 61% одного ядра), потоков до 16.

## PostgreSQL

PostgreSQL 17.12. TPS 1349.5, cache hit 1.000, соединений базы до 31 (активных до 21), размер 16.9 -> 19.0 МиБ.

pg_stat_database за окно: xact_commit 81038, xact_rollback 1, blks_read 0, blks_hit 1199487, tup_returned 2947473, tup_fetched 605030, tup_inserted 5411, tup_updated 677, tup_deleted 3692, temp_files 0, temp_bytes 0, deadlocks 0.

pg_stat_statements недоступен: ERROR: relation "pg_stat_statements" does not exist (SQLSTATE 42P01)

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0 | <= 0.01 | ok |
| delivery.completeness | гейт | 1 | >= 0.99 | ok |
| delivery.messages | гейт | 194 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 652.25 | <= 1000 | ok |
| delivery.from_ack.p99_ms | SLO (предложение) | 879.134 | <= 2000 | ok |
| send.p95_ms | SLO (предложение) | 870.43 | <= 500 | НЕ ПРОЙДЕНА |
