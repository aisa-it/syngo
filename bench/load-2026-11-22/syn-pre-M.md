# Нагрузочный прогон: профиль M против synapse

Сгенерировано `cmd/loadgen`, 2026-11-21T19:40:08Z. Прогон `4e0494b4`, коммит генератора `-`.
Полные данные (ряд ресурсов, все коды) - `report.json` рядом.

## Стенд

| Параметр | Значение |
|---|---|
| Сервер | http://127.0.0.1:38018 (synapse) |
| Сборка сервера | Synapse 1.165.0 CPython 3.13.18 |
| Server | Synapse/1.165.0 |
| Железо | Intel(R) Xeon(R) CPU E5-2620 v4 @ 2.10GHz, 8 CPU, 64310 МиБ RAM, load1 11.3, go1.27.0, PostgreSQL 17.12 |
| db_max_conns | 10 |
| log_level | warning |
| rate_limit | off |
| server_commit | synapse-v1.165.0 |
| server_name | bench.synapse |

## Профиль

| Параметр | Значение |
|---|---|
| Пользователи / комнаты / комнат на пользователя | 500 / 200 / 10 (Ципф 1.0, шифрованных 30%) |
| Модель | закрытая, think time 5s (≈100 действий/с) |
| Ramp / warmup / замер / drain | 1m0s / 1m30s / 5m0s / 15s |
| /sync | timeout 30s, timeline 20, lazy_load_members true |
| Веса действий | send 40, typing 15, receipt 15, messages 6, keys_query 6, media_download 6, media_upload 3, join_leave 3, invite 3, keys_upload 2, create_room 1 |

## Подготовка

615.4 с: пользователей 500 (зарегистрировано 500, вход 0, из кеша 0), комнат создано 200, переиспользовано 0, входов 5000, членств 5000 (самая большая комната 449, самая маленькая 2).

## Итог окна замера

| Метрика | Значение |
|---|---|
| Окно | 301.3 с |
| Действий | 3038 (10.1/с, профиль 100.0/с) |
| Запросов | 5206 (17.3/с) |
| Ошибок (не 2xx и сеть) | 4085, из них 429: 0; доля без 429: 0.7847 |
| Комнат / членств к концу | 200 / 5000 |

## Доставка сообщений

От ответа на PUT /send у отправителя до появления события в /sync получателя (каждый получатель - отдельный замер).

| Метрика | p50 | p95 | p99 | max | замеров |
|---|---|---|---|---|---|
| от ответа PUT, мс | 0.0 | 0.0 | 0.0 | 0.0 | 0 |
| от начала PUT, мс | 0.0 | 0.0 | 0.0 | 0.0 | 0 |

Сообщений 0, ожидалось доставок 0, доставлено 0 (полнота 0.000), раньше ответа отправителю 0, сообщений с недоставкой 0.

## Эндпоинты (клиент)

Латентность на стороне генератора, мс. `sync_longpoll` - long-poll (до timeout), его время - ожидание событий.

| Эндпоинт | Запросов | RPS | p50 | p95 | p99 | max | Ошибок | 429 | Коды |
|---|---|---|---|---|---|---|---|---|---|
| sync_initial | 2012 | 6.68 | 59747 | 59747 | 59747 | 60018 | 1816 | 0 | 200:196 net:1816 |
| send_text | 733 | 2.43 | 60000 | 60000 | 60000 | 60009 | 733 | 0 | net:733 |
| sync_longpoll | 536 | 1.78 | 59747 | 59747 | 59747 | 60009 | 409 | 0 | 200:127 net:409 |
| send_encrypted | 522 | 1.73 | 60000 | 60000 | 60000 | 60014 | 522 | 0 | net:522 |
| typing | 467 | 1.55 | 28047 | 59747 | 59747 | 60003 | 23 | 0 | 200:444 net:23 |
| messages | 182 | 0.60 | 59747 | 59747 | 59747 | 60020 | 149 | 0 | 200:33 net:149 |
| media_download | 180 | 0.60 | 25140 | 45671 | 59747 | 60002 | 3 | 0 | 200:177 net:3 |
| keys_query | 166 | 0.55 | 57990 | 59747 | 59747 | 60005 | 79 | 0 | 200:87 net:79 |
| join | 90 | 0.30 | 60000 | 60000 | 60000 | 60016 | 90 | 0 | net:90 |
| invite | 79 | 0.26 | 60000 | 60000 | 60000 | 60007 | 79 | 0 | net:79 |
| media_upload | 73 | 0.24 | 58570 | 59747 | 59747 | 60003 | 33 | 0 | 200:40 net:33 |
| receipt | 70 | 0.23 | 59747 | 59747 | 59747 | 60011 | 66 | 0 | 200:4 net:66 |
| keys_upload | 62 | 0.21 | 59747 | 59747 | 59747 | 60005 | 49 | 0 | 200:13 net:49 |
| create_room | 34 | 0.11 | 60000 | 60000 | 60000 | 60003 | 34 | 0 | net:34 |

## Сервер

Метрики Synapse (synapse_http_server_*): коды ответов 200:1524 403:29 499:2063 500:4.

Маршруты по числу запросов (серверная гистограмма, интерполяция по корзинам):

| Маршрут | Запросов | p95, мс | p99, мс |
|---|---|---|---|
| GET SyncRestServlet | 2551 | 10000 | 10000 |
| PUT RoomSendEventRestServlet | 1223 | 10000 | 10000 |
| PUT RoomTypingRestServlet | 455 | 10000 | 10000 |
| POST KeyQueryServlet | 191 | 10000 | 10000 |
| GET DownloadResource | 186 | 10000 | 10000 |
| GET RoomMessageListRestServlet | 182 | 10000 | 10000 |
| POST ReceiptRestServlet | 93 | 10000 | 10000 |
| POST JoinRoomAliasServlet | 88 | 10000 | 10000 |
| POST UploadServlet | 88 | 10000 | 10000 |
| POST RoomMembershipRestServlet | 65 | 10000 | 10000 |
| POST KeyUploadServlet | 62 | 10000 | 10000 |
| GET HealthResource | 59 | 4.8 | 5.0 |
| POST RoomCreateRestServlet | 33 | 0.0 | 0.0 |

## Процесс сервера

PID 1128465: RSS 476840 -> 674444 кБ (макс. 674900), CPU 320.4 с за окно (в среднем 106%, пик 121% одного ядра), потоков до 34.

## PostgreSQL

PostgreSQL 17.12. TPS 250.8, cache hit 1.000, соединений базы до 10 (активных до 7), размер 71.0 -> 82.3 МиБ.

pg_stat_database за окно: xact_commit 75462, xact_rollback 0, blks_read 17, blks_hit 19557698, tup_returned 11647924, tup_fetched 9996311, tup_inserted 51245, tup_updated 10507, tup_deleted 9492, temp_files 0, temp_bytes 0, deadlocks 0.

| Запрос (pg_stat_statements) | Вызовов | Всего, мс | Среднее, мс |
|---|---|---|---|
| `WITH RECURSIVE sgs(state_group) AS ( VALUES($1::bigint) UNION ALL SELECT prev_state_group FROM state_group_edges e, sgs …` | 44496 | 203450 | 4.57 |
| `INSERT INTO stream_positions (stream_name, instance_name, stream_id) VALUES ($1, $2, $3) ON CONFLICT (stream_name, insta…` | 396 | 4577 | 11.56 |
| `INSERT INTO worker_read_write_locks (lock_name, lock_key, write_lock, instance_name, token, last_renewed_ts) VALUES($1, …` | 1189 | 2169 | 1.82 |
| `INSERT INTO event_push_actions ( room_id, event_id, user_id, actions, stream_ordering, topological_ordering, notif, high…` | 143 | 1999 | 13.98 |
| `SELECT event.event_id, event.instance_name, event.topological_ordering, event.stream_ordering FROM events AS event WHERE…` | 6459 | 1673 | 0.26 |
| `DELETE FROM presence_stream WHERE stream_id < $1 AND user_id = ANY(ARRAY[$2])` | 235 | 1559 | 6.63 |
| `INSERT INTO user_ips (user_id, access_token, ip, user_agent, device_id, last_seen) VALUES ($1,$2,$3,$4,$5,$6),($7,$8,$9,…` | 7 | 1547 | 221.00 |
| `SELECT * FROM ( SELECT instance_name, stream_ordering, topological_ordering, event_id FROM events LEFT JOIN rejections U…` | 4280 | 1411 | 0.33 |
| `INSERT INTO cache_invalidation_stream_by_instance (stream_id, instance_name, cache_func, keys, invalidation_ts) VALUES($…` | 7 | 1321 | 188.69 |
| `INSERT INTO e2e_one_time_keys_json (user_id, device_id, algorithm, key_id, ts_added_ms, key_json) VALUES ($1,$2,$3,$4,$5…` | 54 | 1046 | 19.38 |

## Проверки

| Проверка | Тип | Значение | Порог | Итог |
|---|---|---|---|---|
| error_rate (без 429) | гейт | 0.785 | <= 0.01 | НЕ ПРОЙДЕНА |
| delivery.completeness | гейт | 0 | >= 0.99 | НЕ ПРОЙДЕНА |
| delivery.messages | гейт | 0 | >= 1 | НЕ ПРОЙДЕНА |
| delivery.from_ack.p95_ms | SLO (предложение) | 0 | <= 1000 | ok |
| delivery.from_ack.p99_ms | SLO (предложение) | 0 | <= 2000 | ok |
| send.p95_ms | SLO (предложение) | 60000.17 | <= 500 | НЕ ПРОЙДЕНА |
