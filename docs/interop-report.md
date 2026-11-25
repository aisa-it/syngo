# Интероп-прогон с живым Synapse

Статус: **прогон выполнен, расхождений нет** (19.11.2026, стенд на чистом томе).

## Результат прогона 19.11.2026

```
preflight: 8 проверок, 0 провалов
сценарий invite/join a.test → synapse.test      OK  состояние 1:1
сценарий invite/join synapse.test → a.test      OK  состояние 1:1 (8 ключей)
сценарий 10 сообщений и порядок                 OK  состояние 1:1
сценарий конфликт power_levels                  OK  состояние 1:1
сценарий leave/rejoin                           OK
сценарий ban/unban                              OK
сценарий backfill после простоя                 OK
PASS: интероп с Synapse — расхождений нет
```

Прогон шёл **с включённой проверкой подписей**: `insecure_skip_signature_verify` из стендового
конфига убран, `interop.sh` отказывается принимать результат, если флаг вернули.
Отдельно проверено, что Synapse не пишет `Invalid event JSON` при разборе наших PDU (было 23
записи за прогон — причина в `state_key: null` у message-событий, исправлено).

Ограничение прошлых прогонов (до 19.11): приемка шла с отключенной проверкой подписей, поэтому
результаты join и обмена событиями приняты только после повторного прогона с проверками.

## Как запустить

```sh
make stand-interop     # deploy/stand/interop.sh — поднимет стенд + Synapse, прогонит сценарии,
                       # сравнит room_state, перезапишет этот файл вердиктом
make stand-interop-keep  # то же, но стенд остаётся поднятым для разбора расхождений
```

Что поднимается:

- `syngod-a` (`a.test`) с HTTPS на 8448 (`config-a-interop.yaml`, `server.tls_*`);
- `syngod-b` (`b.test`) — для базового smoke-сценария;
- `synapse` — `matrixdotorg/synapse:v1.165.0` (pin по референсу ADR-0002, клон `40f63e0`),
  `server_name: synapse.test`, федерация по HTTPS с самоподписанным CA
  (`deploy/stand/interop/certs.sh`), `trusted_key_servers: []` — никаких внешних вызовов.

## Сценарии

| Сценарий | Что проверяется |
|---|---|
| invite/join a.test → synapse.test | приглашение с нашего сервера, входящий `make_join`/`send_join` |
| invite/join synapse.test → a.test | исходящий `Joiner.Join` к живому Synapse |
| 10 сообщений | доставка `m.room.message` федерацией и порядок в `/messages` |
| конфликт power_levels | красный/зелёный конфликт: оба сервера пишут `m.room.power_levels` |
| leave/rejoin | `m.room.member` leave и повторный join через федерацию |
| ban/unban | `m.room.member` ban/unban, авторизация по power level |
| backfill после простоя | наш сервер остановлен, Synapse пишет 5 событий, после старта — догон |

После **каждого** шага `cmd/syngo-interop` сравнивает `room_state` нашего сервера (PostgreSQL)
с `GET /_synapse/admin/v1/rooms/{roomId}/state` и падает с ненулевым кодом при любом
расхождении, выгружая JSON (`deploy/stand/interop-state.json`).

## Критерий приёмки

Все сценарии — состояния совпадают 1:1, ни одного state reset; расхождения описываются как
баги с минимальным примером и оформляются отдельной задачей или ADR.

## Preflight федерации

`deploy/stand/interop.sh` перед сценариями запускает `deploy/stand/preflight.sh` — он проверяет
каждый хоп отдельно и печатает вердикт: hosts на хосте (`a.test`/`b.test` → 127.0.0.2,
`synapse.test` → 127.0.0.1), TLS до Synapse и до `a.test`, **доступность
`https://a.test:8448/_matrix/key/v2/server` из контейнера Synapse** (именно здесь возникал 401
«Failed to find any key»), а также видимость `synapse.test:8448` из контейнера A. Провал любого
хопа останавливает прогон с конкретной причиной, а не общим «invite не доставлен».

## Сеть стенда

Synapse `v1.165.0` работает в **host-сети**: его Twisted-резолвер не читает `/etc/hosts`
контейнера и не видит docker-алиасы (`DNSLookupError` на `a.test`, хотя `getaddrinfo` в
контейнере отвечал). Поэтому участники стенда резолвятся через `/etc/hosts` хоста
(`a.test`, `b.test`, `synapse.test` → 127.0.0.1 — `interop.sh` добавляет записи сам),
Syngod A публикует федеративный 8448 на хост, а PostgreSQL — только на `127.0.0.1:5432`.

## Ограничения стенда

- Кластер PostgreSQL стенда создаётся с локалью по умолчанию (не `C`), поэтому в
  `deploy/stand/synapse/homeserver.yaml` включён `allow_unsafe_locale: true`. Для
  синтетических данных тестового контура это безопасно, в проде нужен `initdb` с локалью `C`.
- Приватный ключ `deploy/stand/certs/synapse.test.key` доступен на чтение (`chmod 644`),
  иначе Synapse (uid 991) не прочитает его из read-only bind-mount. Это допустимо только для
  самоподписанного сертификата стенда.

## Client API для сценариев

`PUT /rooms/{id}/state/{type}/{key}`, `GET /rooms/{id}/state`, `POST /kick`, `/ban`, `/unban`
и `PUT /redact/{eventId}/{txnId}` реализованы (плюс `GET /rooms/{id}/members`,
`/joined_members`, `/aliases`, `/directory/room/{alias}`, `/refresh`, `/register/available`,
`/delete_devices`). Сценарии конфликта power_levels и ban/unban больше не упираются
в 404 на нашем сервере.
