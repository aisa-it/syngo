# Нагрузочный прогон: профиль S против syngo

Сгенерировано `cmd/loadgen`, 2026-11-21T18:02:50Z. Прогон `e4179acc`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38008 (syngo) |
| Сборка сервера | - - go1.27.4 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 11.7, go1.27.4, PostgreSQL 17.12 |
| db_max_conns | 10 |
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

22.7 с: пользователей 50 (зарегистрировано 50, вход 0, из кеша 0), комнат создано 20, переиспользовано 0, входов 250, членств 250 (самая большая комната 40, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 60.2 с |
| Действий | 528 (8.8/с, профиль 10.0/с) |
| Запросов | 6041 (100.3/с) |
| Ошибок (не 2xx и сеть) | 0, из них 429: 0; доля без 429: 0.0000 |
| Комнат / членств к концу | 24 / 276 |
| sync_limited_timelines | 10 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 198 | 627 | 3369 | 5796 | 3837 |
| от начала PUT, мс | 573 | 6627 | 8331 | 8733 | 3837 |

Сообщений 197, ожидалось доставок 3838, доставлено 3837 (полнота 1.000), раньше ответа отправителю 38, сообщений с недоставкой 1.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 5509 | 91.45 | 425 | 1349 | 4495 | 8186 | 0 | 0 | 200:5509 |
| send_encrypted | 115 | 1.91 | 359 | 4916 | 8006 | 8062 | 0 | 0 | 200:115 |
| typing | 92 | 1.53 | 9.2 | 2196 | 3359 | 3359 | 0 | 0 | 200:92 |
| send_text | 83 | 1.38 | 434 | 6181 | 7900 | 7900 | 0 | 0 | 200:83 |
| receipt | 80 | 1.33 | 104 | 3648 | 7751 | 7751 | 0 | 0 | 200:80 |
| media_download | 35 | 0.58 | 74.5 | 2426 | 2778 | 2778 | 0 | 0 | 200:35 |
| keys_query | 34 | 0.56 | 41.4 | 1490 | 2064 | 2064 | 0 | 0 | 200:34 |
| messages | 32 | 0.53 | 107 | 2262 | 2450 | 2453 | 0 | 0 | 200:32 |
| media_upload | 15 | 0.25 | 44.9 | 4349 | 4349 | 4349 | 0 | 0 | 200:15 |
| join_invite | 14 | 0.23 | 456 | 8331 | 8331 | 8364 | 0 | 0 | 200:14 |
| keys_upload | 11 | 0.18 | 277 | 4029 | 4029 | 4036 | 0 | 0 | 200:11 |
| join | 7 | 0.12 | 429 | 1360 | 1360 | 1360 | 0 | 0 | 200:7 |
| invite | 6 | 0.10 | 120 | 1322 | 1322 | 1325 | 0 | 0 | 200:6 |
| leave | 5 | 0.08 | 129 | 742 | 742 | 743 | 0 | 0 | 200:5 |
| create_room | 3 | 0.05 | 845 | 915 | 915 | 919 | 0 | 0 | 200:3 |

## Сервер

| Метрика | Значение |
|---|---|
| /sync без long-poll p50 / p95 / p99, мс | 116 / 1335 / 4675 |
| 429 (syngo_rate_limited_total) | 0 |
| Пул БД: выдач с ожиданием / ожидание, с / предел | 45084 / 748.907 / 10 |
| GC: циклов / паузы, с / куча в конце, МиБ | 92 / 0.108 / 9.6 |
| Макс. long-poll / горутин | 10 / 162 |
| Коды ответов | 200:6038 |

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET /_matrix/client/v3/sync | 5505 | 1952 | 4814 |
| PUT /_matrix/client/v3/rooms/{roomId}/send/{eventType}/{txnId} | 198 | 5500 | 9100 |
| PUT /_matrix/client/v3/rooms/{roomId}/typing/{userId} | 93 | 1705 | 3838 |
| POST /_matrix/client/v3/rooms/{roomId}/receipt/{receiptType}/{eventId} | 80 | 4375 | 8000 |
| GET /_matrix/client/v1/media/download/{serverName}/{mediaId} | 35 | 2275 | 4125 |
| POST /_matrix/client/v3/keys/query | 34 | 1650 | 2330 |
| GET /_matrix/client/v3/rooms/{roomId}/messages | 32 | 2020 | 2404 |
| POST /_matrix/media/v3/upload | 15 | 4062 | 4812 |
| POST /_matrix/client/v3/rooms/{roomId}/join | 14 | 8250 | 9650 |
| POST /_matrix/client/v3/keys/upload | 11 | 4312 | 4862 |
| POST /_matrix/client/v3/join/{roomIdOrAlias} | 7 | 2238 | 2448 |
| POST /_matrix/client/v3/rooms/{roomId}/invite | 6 | 2050 | 2410 |
| POST /_matrix/client/v3/rooms/{roomId}/leave | 5 | 875 | 975 |
| POST /_matrix/client/v3/createRoom | 3 | 962 | 992 |

## Процесс сервера

PID 1042649: RSS 41108 -> 44740 кБ (макс. 45116), CPU 24.6 с за окно (в среднем 41%, пик 77% одного ядра), потоков до 15.

## PostgreSQL

PostgreSQL 17.12. TPS 1191.9, cache hit 1.000, соединений базы до 11 (активных до 9), размер 13.3 -> 15.8 МиБ.

pg_stat_database за окно: xact_commit 71575, xact_rollback 1, blks_read 0, blks_hit 1059879, tup_returned 2236655, tup_fetched 425254, tup_inserted 5308, tup_updated 556, tup_deleted 2293, temp_files 0, temp_bytes 0, deadlocks 0.

pg_stat_statements недоступен: ERROR: relation "pg_stat_statements" does not exist (SQLSTATE 42P01)

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0 | <= 0.01 | ok |
| delivery.completeness | гейт | 1 | >= 0.99 | ok |
| delivery.messages | гейт | 197 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 626.799 | <= 1000 | ok |
| delivery.from_ack.p99_ms | SLO (предложение) | 3368.53 | <= 2000 | НЕ ПРОЙДЕНА |
| send.p95_ms | SLO (предложение) | 4916.462 | <= 500 | НЕ ПРОЙДЕНА |
