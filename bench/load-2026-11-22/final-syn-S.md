# Нагрузочный прогон: профиль S против synapse

Сгенерировано `cmd/loadgen`, 2026-11-22T04:33:36Z. Прогон `61b9fafa`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38018 (synapse) |
| Сборка сервера | Synapse 1.165.0 CPython 3.13.18 |
| Server | Synapse/1.165.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 3.4, go1.27.0, PostgreSQL 17.12 |
| db_max_conns | 10 |
| log_level | warning |
| rate_limit | off |
| server_commit | synapse-v1.165.0 |
| server_name | bench.synapse |

## Профиль

| Параметр | Значение |
|---|---|
| Пользователи / комнаты / комнат на пользователя | 50 / 20 / 5 (Ципф 1.0, шифрованных 30%) |
| Модель | закрытая, think time 5s (≈10 действий/с) |
| Ramp / warmup / замер / drain | 15s / 20s / 3m0s / 10s |
| /sync | timeout 30s, timeline 20, lazy_load_members true |
| Веса действий | send 40, typing 15, receipt 15, messages 6, keys_query 6, media_download 6, media_upload 3, join_leave 3, invite 3, keys_upload 2, create_room 1 |

## Подготовка

22.5 с: пользователей 50 (зарегистрировано 0, вход 50, из кеша 0), комнат создано 0, переиспользовано 20, входов 250, членств 250 (самая большая комната 40, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 180.4 с |
| Действий | 723 (4.0/с, профиль 10.0/с) |
| Запросов | 3206 (17.8/с) |
| Ошибок (не 2xx и сеть) | 15, из них 429: 0; доля без 429: 0.0047 |
| Комнат / членств к концу | 152 / 661 |
| sync_limited_timelines | 32 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 3648 | 6896 | 8583 | 12524 | 4209 |
| от начала PUT, мс | 14836 | 34911 | 41345 | 44690 | 4209 |

Сообщений 259, ожидалось доставок 4209, доставлено 4209 (полнота 1.000), раньше ответа отправителю 2, сообщений с недоставкой 0.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_longpoll | 2423 | 13.43 | 3302 | 5540 | 8498 | 14265 | 0 | 0 | 200:2423 |
| send_text | 191 | 1.06 | 10898 | 29773 | 38564 | 40067 | 0 | 0 | 200:191 |
| typing | 114 | 0.63 | 514 | 1566 | 1929 | 2278 | 0 | 0 | 200:114 |
| receipt | 99 | 0.55 | 2308 | 3648 | 9021 | 9030 | 0 | 0 | 200:99 |
| send_encrypted | 87 | 0.48 | 11919 | 40936 | 44328 | 44495 | 0 | 0 | 200:87 |
| messages | 52 | 0.29 | 2218 | 5116 | 12124 | 12124 | 0 | 0 | 200:52 |
| sync_initial | 45 | 0.25 | 43889 | 46128 | 46405 | 46405 | 0 | 0 | 200:45 |
| media_download | 44 | 0.24 | 514 | 1270 | 1460 | 1463 | 0 | 0 | 200:44 |
| keys_query | 41 | 0.23 | 943 | 6432 | 8006 | 8017 | 0 | 0 | 200:41 |
| join_invite | 24 | 0.13 | 16226 | 31605 | 33482 | 33482 | 0 | 0 | 200:24 |
| media_upload | 21 | 0.12 | 1162 | 2131 | 3436 | 3443 | 0 | 0 | 200:21 |
| leave | 19 | 0.10 | 10473 | 14473 | 14473 | 14473 | 0 | 0 | 200:19 |
| keys_upload | 17 | 0.09 | 2706 | 8744 | 8744 | 8744 | 0 | 0 | 200:17 |
| invite | 16 | 0.09 | 5431 | 16641 | 16641 | 16641 | 14 | 0 | 200:2 403:14 |
| create_room | 10 | 0.06 | 48001 | 59747 | 59747 | 60000 | 1 | 0 | 200:9 net:1 |
| join | 3 | 0.02 | 17571 | 19997 | 19997 | 20090 | 0 | 0 | 200:3 |

## Сервер

Метрики Synapse (synapse_http_server_*): коды ответов 200:3252 403:14.

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET SyncRestServlet | 2469 | 6753 | 10000 |
| PUT RoomSendEventRestServlet | 274 | 10000 | 10000 |
| PUT RoomTypingRestServlet | 113 | 1781 | 2356 |
| POST ReceiptRestServlet | 101 | 4912 | 7492 |
| POST RoomMembershipRestServlet | 60 | 10000 | 10000 |
| GET RoomMessageListRestServlet | 53 | 5500 | 10000 |
| GET HealthResource | 52 | 4.8 | 5.0 |
| GET DownloadResource | 45 | 1825 | 2365 |
| POST KeyQueryServlet | 40 | 6188 | 8975 |
| POST UploadServlet | 20 | 2494 | 4475 |
| POST KeyUploadServlet | 14 | 9292 | 9858 |
| POST RoomCreateRestServlet | 11 | 10000 | 10000 |

## Процесс сервера

PID 1745253: RSS 293028 -> 330552 кБ (макс. 330788), CPU 225.6 с за окно (в среднем 125%, пик 131% одного ядра), потоков до 31.

## PostgreSQL

PostgreSQL 17.12. TPS 325.0, cache hit 0.999, соединений базы до 10 (активных до 8), размер 116.9 -> 148.9 МиБ.

pg_stat_database за окно: xact_commit 58770, xact_rollback 0, blks_read 8404, blks_hit 7224124, tup_returned 5508357, tup_fetched 2961856, tup_inserted 84252, tup_updated 15839, tup_deleted 41301, temp_files 0, temp_bytes 0, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `WITH RECURSIVE sgs(state_group) AS ( VALUES($1::bigint) UNION ALL SELECT prev_state_group FROM state_group_edges e, sgs …` | 11069 | 56189 | 5.08 |
| `INSERT INTO event_push_actions ( room_id, event_id, user_id, actions, stream_ordering, topological_ordering, notif, high…` | 423 | 3092 | 7.31 |
| `SELECT * FROM ( SELECT instance_name, stream_ordering, topological_ordering, event_id FROM events LEFT JOIN rejections U…` | 9718 | 2792 | 0.29 |
| `INSERT INTO event_push_summary (user_id, room_id, thread_id, notif_count, unread_count, stream_ordering) VALUES ($1,$2,$…` | 136 | 1713 | 12.60 |
| `SELECT notif_count, COALESCE(unread_count, $1), thread_id FROM event_push_summary LEFT JOIN ( SELECT thread_id, MAX(even…` | 5339 | 1193 | 0.22 |
| `INSERT INTO stream_positions (stream_name, instance_name, stream_id) VALUES ($1, $2, $3) ON CONFLICT (stream_name, insta…` | 470 | 1153 | 2.45 |
| `SELECT e.event_id, e.room_id, e.type, se.state_key, r.reason FROM events AS e LEFT JOIN state_events se USING (event_id)…` | 5366 | 1092 | 0.20 |
| `SELECT COUNT(CASE WHEN notif = $1 THEN $2 END), COUNT(CASE WHEN unread = $3 THEN $4 END), thread_id FROM event_push_acti…` | 4925 | 895 | 0.18 |
| `SELECT COUNT(CASE WHEN notif = $1 THEN $2 END), COUNT(CASE WHEN unread = $3 THEN $4 END), thread_id FROM event_push_acti…` | 4023 | 677 | 0.17 |
| `DELETE FROM worker_read_write_locks WHERE lock_name = $1 AND lock_key = $2 AND token = $3` | 767 | 653 | 0.85 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0.005 | <= 0.01 | ok |
| delivery.completeness | гейт | 1 | >= 0.99 | ok |
| delivery.messages | гейт | 259 | >= 1 | ok |
| delivery.from_ack.p95_ms | SLO (предложение) | 6895.716 | <= 1000 | НЕ ПРОЙДЕНА |
| delivery.from_ack.p99_ms | SLO (предложение) | 8583.207 | <= 2000 | НЕ ПРОЙДЕНА |
| send.p95_ms | SLO (предложение) | 29772.913 | <= 500 | НЕ ПРОЙДЕНА |
