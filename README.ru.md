# syngo

[![License: AGPL v3+](https://img.shields.io/badge/License-AGPL_v3%2B-blue.svg)](https://www.gnu.org/licenses/agpl-3.0.html)

English version: [README.md](README.md)

Homeserver Matrix на Go: совместимый по Client-Server API сервер без Python-рантайма.
Поддерживается один процесс syngod на базу: второй экземпляр не стартует (advisory-блокировка,
ADR-0009); горизонтальное масштабирование - после выноса состояния из памяти процесса.
Проект портирует поведение эталонного Synapse (Python/Twisted) и сверяется со спецификацией
Matrix и Complement.

Основные функции клиентского API, синхронизации и миграции с Synapse реализованы.
Федеративный обмен проверен на стенде с Synapse. Перед переносом рабочей инсталляции
проведите репетицию на копии данных (`docs/migration-rehearsal.md`, `docs/migration-runbook.md`).

Что подтверждено фактом (артефакты в репозитории):

- **Интероп с живым Synapse:** `docs/interop-report.md` — все сценарии 1:1 (join в обе стороны,
  10 сообщений, конфликт `power_levels`, leave/rejoin, ban/unban, backfill), прогон **с включённой
  проверкой подписей** (без `insecure_skip_signature_verify`).
- **CI:** GitHub Actions (`.github/workflows/ci.yml`) на каждый push в `main` и pull request: сборка
  бинаря и образа, `go vet` и golangci-lint, тесты с `-race` и coverage-ratchet по пакетам,
  интеграционные тесты на PostgreSQL (с гейтом числа SQL-запросов `/sync`), тесты гейтов;
  Complement - отдельный workflow с ручным запуском (раздел «CI»).
- **Complement.** В прогоне 25 ноября 2026 года из 553 зарегистрированных проверок прошли 340,
  завершились ошибкой 206, пропущены 7. Все результаты совпали с замером 21 ноября.
  У тестового образа нет HTTPS на порту 8448. После остановки тестового клиента ещё 77 сценариев
  федерации остались без результата. [Результаты прогона](test/complement/results.jsonl)

Возможности:

- **Сервер:** `GET /capabilities` (room versions, join rules) — нужен Element Web.
- **Auth:** `/register` (UIA dummy), `/login` (password + refresh), `/logout`, `/logout/all`,
  `/account/whoami`, `/account/password` (UIA, logout_devices), `/account/deactivate`,
  `/delete_devices`, `/refresh`, `/register/available`.
- **Комнаты:** `/createRoom`, `join/invite/leave/kick/ban/unban`, `/send`, `/messages`, `/event`,
  `/members`, `/joined_members`, `/aliases`, `PUT/GET /state` (+ валидация `canonical_alias`),
  `PUT /redact` (неразрушающая redaction).
- **Sync:** `/sync` с long-poll, to-device, account data, device lists, presence-заглушкой;
  simplified sliding sync (MSC4186).
- **Ключи/E2EE-транспорт:** `/keys/upload|query|claim|changes`, `/keys/device_signing/upload`.
- **Медиа:** upload/download (v1 и legacy v3), `preview_url`, локальное хранилище.
- **Pushers и поиск:** `GET/POST /pushers`, `POST /search`.
- **Федерация:** ed25519-ключи и `/_matrix/key/v2/server`, X-Matrix-подпись (включая
  канонический JSON без HTML-экранирования), resolver (well-known→SRV→8448→443), send/state/
  state_ids/backfill/get_missing_events/event, входящие и исходящие invite/join/leave,
  `send_join`/`send_leave` v1+v2, auth rules v10/v11, state resolution v2 (mainline, auth diff,
  differential-тесты из Synapse), outbox с ретраями и метриками.
- **Эксплуатация:** `/health`, `/health/ready` (БД + версия схемы + дренаж), `/metrics`
  на служебном листенере `server.ops_addr` (sync p50/p95/p99, outbox, push, 429, state resolution), graceful shutdown, backup/restore-скрипты,
  прод-compose и systemd-юнит.
- **Инструменты миграции:** `syngo-import*`, `syngo-export`, `synapse-fixture`, `syngo-interop`.

Не готово: полное прохождение Complement (раздел «Complement и ratchet»).

## Версии и совместимость с Synapse

Версия - `v1.<minor Synapse>.<патч syngo>`: `v1.165.0` совместима с Synapse 1.165. Minor
совпадает с версией Synapse, с которой проверена совместимость; патч - релизы syngo на той
же базе. Переход на новую версию Synapse (после перепроверки) - новый minor: `v1.<N>.0`.

| syngo | Synapse | Что проверено |
|---|---|---|
| v1.165.x | 1.165 | перенос БД (`syngo-export`, `syngo-import`, фикстуры Synapse 1.165), федерация (интероп-стенд с Synapse 1.165), Client-Server API (Element Web, Element X - simplified sliding sync) |

Версия видна в `syngod -version`, `GET /_matrix/federation/v1/version` (`{"name": "syngo",
"version": "1.165.0"}`) и метрике `syngo_build_info{version,synapse_compat}`; задается тегом
git при сборке (`make build`, Docker `--build-arg VERSION`). Версия совместимости - константа
`internal/build.SynapseCompat`, тест сверяет ее с образом Synapse стендов и фикстурами.
История - `CHANGELOG.md`.

## Требования

Инструменты разработки ставятся `make tools` **проектным тулчейном** `GOTOOLCHAIN=go1.27.4`
(переопределяется переменной окружения): на машине с базовым Go 1.24 `go install` иначе
собирал `golangci-lint` тулчейном из его `go.mod` и линтер отказывался анализировать
проект (`go1.26 < targeted 1.27`). `govulncheck` запинен на v1.9.0: v1.10.0 падает на
stdlib go1.27 («generic method requires go1.27»).

- Go 1.27+
- PostgreSQL 16+ (необязательно: без DSN сервис поднимается, readiness отвечает 503)
- для `make check`: `golangci-lint` v2, `gofumpt`, `goimports`, `govulncheck` (`make tools`)

## Быстрый старт

```bash
git clone https://github.com/aisa-it/syngo.git
cd syngo
make tools        # один раз: линтеры и goimports
make run          # слушает :8008 (dev: server_name localhost, регистрация открыта)

# в другом терминале
curl -s localhost:8008/health
curl -s localhost:8008/_matrix/client/versions
```

Ответы:

```json
{"status":"ok","version":"dev","commit":"unknown","built_at":"unknown","uptime_seconds":1.2}
{"versions":["v1.1","v1.2",...,"v1.11"],"unstable_features":{}}
```

## Локальный стенд федерации

Два независимых сервера SYNGO в docker-сети, чтобы проверять федерацию не «на словах»:
`a.test` (host `127.0.0.1:18008`) и `b.test` (`127.0.0.1:18009`), отдельные БД `syngo_a`/`syngo_b`,
свои ключи подписи и тома медиа. Внутри стенда федерация идёт по HTTP (`federation.scheme: http`),
наружу порты не маршрутизируются.

```sh
make stand-up      # .env, ключи ed25519, БД + миграции, пользователи, комната и приглашение
make stand-smoke   # инвайт → join через федерацию → 5 сообщений → порядок → leave → рестарт b → идемпотентность
make stand-down    # остановить (данные сохраняются)
make stand-reset   # снести всё: контейнеры, тома, ключи, состояние
make stand-logs    # логи обоих серверов
```

Что делает `make stand-up` (`deploy/stand/bootstrap.sh`, идемпотентен):

1. создаёт `deploy/stand/.env` из `.env.example` (chmod 600);
2. генерирует ключи `deploy/stand/keys/{a,b}.key` через `cmd/federation-keygen` (chmod 600, в git не попадают);
3. поднимает PostgreSQL 17 и накатывает миграции в обе БД;
4. запускает `syngod-a`/`syngod-b` и ждёт `/health`;
5. регистрирует `alice@a.test` и `bob@b.test`, создаёт комнату и приглашает bob;
6. пишет токены и `room_id` в `deploy/stand/.stand-state.env` (chmod 600).

`make stand-smoke` (`deploy/stand/smoke.sh`) возвращает ненулевой код, если сообщение не доехало,
нарушен порядок (проверка через `/messages`, `/sync` и напрямую по БД `b.test`), появился дубль
после повторного `send` с тем же `txn_id`, либо доставка не восстановилась после рестарта `syngod-b`.

Конфигурация стенда: `deploy/stand/config-a.yaml`, `deploy/stand/config-b.yaml`
(`allow_hosts` друг на друга, rate limit выключен, уровень логов debug). TLS-вариант для проверки
self-signed сертификатов: `federation.scheme: https` + `federation.insecure_skip_verify: true`
(в `env: prod` эта опция запрещена валидацией).

Требования: docker + docker compose v2, curl, python3 (разбор JSON в скриптах).

### Интероп с живым Synapse

Третьим сервером на стенде поднимается Synapse 1.165.0 (`deploy/stand/docker-compose.synapse.yml`),
федерация — по HTTPS с самоподписанным CA (`deploy/stand/interop/certs.sh`). Прогон одной командой:

```sh
make stand-interop   # сценарии + сверка room_state против admin API Synapse, отчёт в docs/interop-report.md
```

**Только для стенда:** самоподписанные ключи `deploy/stand/certs/{synapse.test,a.test}.key`
монтируются в контейнеры read-only, поэтому `interop.sh` выставляет им `chmod 644` (иначе
Synapse под uid 991 и наш syngod под uid 10001 не прочитают ключи). В проде так делать нельзя: приватный ключ хранится `0600` и принадлежит
пользователю сервиса, а TLS-терминатор получает файл через секрет-хранилище.

`cmd/syngo-interop` читает наше `room_state` из PostgreSQL, состояние Synapse — через
`GET /_synapse/admin/v1/rooms/{roomId}/state`, сравнивает по `(type, state_key)` и возвращает
ненулевой код при любом расхождении. Для HTTPS-федерации у сервера появились
`server.tls_cert_file`/`server.tls_key_file` (env `SYNGOD_TLS_CERT_FILE`/`SYNGOD_TLS_KEY_FILE`).

## Запуск в проде

Продовый образ - distroless nonroot (`Dockerfile`, последняя стадия `prod`: `docker build .`
собирает именно ее). В образе два бинаря: `/syngod` и `/syngo-migrate`, SQL-миграции встроены
в оба. Healthcheck образа - `/syngod -healthcheck` (GET `/health/ready`, curl в образе нет).

Развертывание одной командой с чистой машины (нужны docker и compose v2):

```bash
cp deploy/prod/.env.example deploy/prod/.env
# заполнить: SYNGOD_SERVER_NAME (публичный домен), POSTGRES_PASSWORD (openssl rand -hex 24),
# SYNGOD_SYNC_TOKEN_SECRET и SYNGOD_REGISTRATION_SHARED_SECRET (openssl rand -hex 32)
docker compose -f deploy/prod/docker-compose.yml up -d --build --wait
```

Что делает compose (`deploy/prod/docker-compose.yml`):

| Сервис | Роль |
|---|---|
| `postgres` | PostgreSQL 17, том `pgdata`, порт наружу не публикуется |
| `migrate` | одноразовый `syngo-migrate -up` из того же образа, после healthy postgres |
| `syngod` | стартует после успешных миграций; `env: prod` (`deploy/prod/config.yaml`), секреты из `.env`, том `media` (владелец nonroot), порт `127.0.0.1:8008` |

Образ по умолчанию собирается из исходников с тегом `syngo/syngod:local`; в проде закрепите
версию: `SYNGO_IMAGE=registry.example.org/syngo/syngod:<tag>`. TLS-терминатор (nginx/traefik)
ставится перед `127.0.0.1:8008`. Федерация выключена; как включить - комментарий в compose-файле.

Первый пользователь и администратор (регистрация закрыта) - через общий секрет,
совместимо с `register_new_matrix_user` из Synapse:

```bash
register_new_matrix_user -k "$SYNGOD_REGISTRATION_SHARED_SECRET" -u admin -a http://127.0.0.1:8008
```

Проверки:

- `deploy/prod/smoke.sh` - после деплоя: `/health`, `/health/ready`, `/_matrix/client/versions`,
  `/register` закрыт, регистрация оператором (`SMOKE_SHARED_SECRET`) или вход существующим
  пользователем (`SMOKE_USER`/`SMOKE_PASSWORD`), комната, сообщение, `/sync`, upload/download
  медиа; smoke-пользователь в конце деактивируется.
  `BASE=http://127.0.0.1:8008 SMOKE_SHARED_SECRET=... deploy/prod/smoke.sh`
- `deploy/prod/smoke-compose.sh` - сквозная проверка: `docker build .`, отдельный compose-проект
  со сгенерированными секретами на порту `SMOKE_PORT` (по умолчанию 18008), smoke, пересоздание
  `syngod` и `migrate`, затем `down -v`.

Старт `syngod` прерывается с понятной ошибкой, если: конфиг невалиден (см. «Конфигурация»), схема
БД не накатана, отстает, новее бинаря или dirty, каталог медиа недоступен на запись,
на той же базе уже работает другой syngod: ошибка
`startup: store: database is already used by another syngod instance (application_name=syngod-instance host=... pid=... client=... since=...)`
после 30 с ожидания (перекрытие при рестарте). Одна реплика: в compose не задавать `scale`,
в Kubernetes - `replicas: 1` и стратегия `Recreate` (ADR-0009).

- `deploy/prod/syngod.service` - systemd-юнит с `EnvironmentFile=/etc/syngo/env`, `SIGTERM`,
  `TimeoutStopSec=30` и базовой изоляцией (`ProtectSystem=strict`, без capabilities). Перед
  запуском новой версии: `syngo-migrate -dsn "$SYNGOD_DATABASE_DSN" -up`.

**Graceful shutdown.** По SIGTERM сервер: перестаёт ждать в long-poll `/sync` (клиенты сразу
получают текущую позицию), дренирует исходящую федеративную очередь (`outbox.Tick` до пустой
или до `server.shutdown_timeout`), затем закрывает HTTP с таймаутом. `/health/ready` в это
время отвечает 503.

## Мониторинг

Служебные эндпоинты - на отдельном листенере `server.ops_addr`
(`SYNGOD_OPS_ADDR`, в прод-compose `:9108`, опубликован только на loopback): `GET /metrics`,
подробные `/health` (версия, коммит) и `/health/ready` (причины отказа), `/debug/pprof/` при
`server.pprof: true`. На публичном листенере - только краткие `/health` и `/health/ready`
(`{"status":"ok"}`, проверки без текста ошибок); `/metrics` там есть лишь в dev/test без
`ops_addr`. Логи запросов содержат шаблон маршрута (`route`), а не путь; заголовки
Authorization, query и тела запросов не логируются, логгер процесса дополнительно маскирует
секреты в атрибутах.

`GET /metrics` - текстовый формат Prometheus. Ключевые метрики (полный список - `docs/operations.md`):

| Метрика | Что показывает |
|---|---|
| `syngo_ready`, `syngo_ready_check{check}` | готовность как `/health/ready`, по проверкам |
| `syngo_http_requests_total{method,route,code}`, `syngo_http_request_duration_seconds` | запросы по шаблону маршрута и коду, гистограмма длительности |
| `syngo_sync_duration_seconds` | гистограмма обработки `/sync` без ожидания long-poll |
| `syngo_sync_requests_total`, `syngo_sync_longpoll_waiters` | число запросов и ожидающих long-poll |
| `syngo_db_pool_*`, `syngo_db_size_bytes` | пул PostgreSQL, размер базы |
| `syngo_media_disk_free_bytes`, `syngo_media_disk_size_bytes` | место на томе медиа |
| `syngo_federation_outbox_depth`, `..._oldest_seconds`, `..._attempts_total` | исходящая очередь федерации |
| `syngo_push_notifications_total{result}`, `syngo_push_queued_notifications`, `syngo_push_active_queues` | доставка и очередь push |
| `syngo_instance_lock_held` | блокировка экземпляра |
| `syngo_rate_limited_total` | ответы 429 |
| `syngo_state_resolution_*` | длительность, конфликты и изменения состояния |
| `syngo_build_info`, `process_start_time_seconds`, `go_*` | версия, старт, рантайм Go |

`/health/ready` учитывает БД, версию схемы (последняя встроенная миграция), блокировку
экземпляра и признак завершения работы. Очередь федерации в readiness не входит:
один недоступный пир не выводит инстанс из балансировки; застрявшая доставка - алерт
`SyngoFederationOutboxStuck`.

Готовые конфиги: `deploy/monitoring/prometheus.yml`, `deploy/monitoring/grafana-dashboard.json`
(16 панелей) и алерты `deploy/monitoring/alerts.yml` с тестами `alerts_test.yml`
(`promtool test rules`): доступность и готовность, 5xx, p99 API и `/sync`, место на томах,
пул БД, бэкап не выполнялся или упал, очереди push и федерации. Действия по алертам -
`docs/operations.md`, раздел 7.

## Backup и restore

Runbook - `docs/operations.md`. Бэкап - единый снимок: `pg_dump --snapshot`,
счетчики таблиц и список файлов медиа из одного экспортированного снимка; плюс архив медиа,
секреты и конфиг (0600), `SHA256SUMS`, шифрование gpg, вынос, ротация, метрики для алертов.

```sh
# docker compose: бэкап, проверка во временную базу, восстановление в пустой проект
deploy/prod/backup-compose.sh backup
deploy/prod/backup-compose.sh verify /var/backups/syngo/syngo-<stamp>
deploy/prod/backup-compose.sh restore /var/backups/syngo/syngo-<stamp>.tar.gpg

# без docker
SYNGOD_DATABASE_DSN=postgres://... MEDIA_DIR=/var/syngo/media \
  BACKUP_SECRET_FILES="/etc/syngo/env /etc/syngo/config.yaml" deploy/backup.sh
SYNGOD_DATABASE_DSN=postgres://... deploy/verify-restore.sh /var/backups/syngo/syngo-<stamp>
SYNGOD_DATABASE_DSN=postgres://.../empty MEDIA_DIR=/var/syngo/media deploy/restore.sh /var/backups/syngo/syngo-<stamp>

# сквозная проверка на docker: бэкап под записью -> verify -> restore в чистый проект
deploy/prod/backup-restore-compose.sh
```

Расписание - `deploy/prod/syngo-backup.timer` (ежесуточно). **RPO** - интервал таймера
(сутки), **RTO** - `restore.sh` и старт сервиса: минуты на объемах до десятков ГБ.

## Конфигурация

Порядок: значения по умолчанию → YAML (`-config` / `$SYNGOD_CONFIG`) → переменные
окружения. Пример — `config.example.yaml`, переменные — `.env.example`.

| Переменная | Смысл |
|---|---|
| `SYNGOD_CONFIG` | путь к YAML-конфигу |
| `SYNGOD_ENV` | `dev` / `test` / `prod`; по умолчанию `prod` |
| `SYNGOD_SERVER_ADDR` | адрес HTTP-сервера (по умолчанию `:8008`) |
| `SYNGOD_MATRIX_SERVER_NAME` | имя сервера, только hostname; обязательно, дефолта нет |
| `SYNGOD_DATABASE_DSN` | DSN PostgreSQL; пусто допустимо только в dev/test |
| `SYNGOD_SYNC_TOKEN_SECRET` | секрет токенов `/sync` |
| `SYNGOD_AUTH_REGISTRATION` | `true` - открыть регистрацию (по умолчанию закрыта) |
| `SYNGOD_REGISTRATION_SHARED_SECRET` | секрет регистрации оператором (`/_synapse/admin/v1/register`) |
| `SYNGOD_VOIP_SECRET` | static-auth-secret coturn |
| `SYNGOD_MEDIA_DIRECTORY` | каталог медиа |
| `SYNGOD_TRUSTED_PROXIES` | доверенные прокси через запятую (CIDR или IP), см. «IP клиента за прокси» |
| `SYNGOD_LOG_LEVEL` | `debug` / `info` / `warn` / `error` |
| `SYNGOD_LOG_FORMAT` | `json` / `text` |

Неизвестные ключи в YAML считаются ошибкой (`KnownFields`), конфиг валидируется
до старта: сервис падает сразу, а не в первом запросе.

Безопасность по умолчанию. Окружение по умолчанию - `prod` (пустой `env` тоже prod);
вне `dev`/`test`:

| Правило | Ошибка валидации |
|---|---|
| `matrix.server_name` задан и не `localhost`, `*.localhost`, loopback | `matrix.server_name ... is not allowed outside dev/test` |
| `database.dsn` задан | `database.dsn is required outside dev/test` |
| `sync.token_secret` задан, не короче 32 символов | `sync.token_secret is required` / `is too short` |
| `auth.registration_shared_secret`, если задан, не короче 32 символов | `auth.registration_shared_secret is too short` |
| `federation.insecure_skip_verify`, `insecure_skip_signature_verify`, `send_full_member_state`, `push.allow_http`, `push.allow_private` выключены | `<флаг> запрещен вне dev/test` |

Регистрация закрыта по умолчанию (`auth.registration_open: false`). Секреты в лог не попадают:
при старте логируется `Config.String()` (адрес, server_name, факт наличия DSN, уровень логов).

Локальная разработка - явный dev: `SYNGOD_ENV=dev SYNGOD_MATRIX_SERVER_NAME=localhost`
(`make run` задает их сам).

## Команды

| Команда | Что делает |
|---|---|
| `make build` | статический бинарь `bin/syngod` с version/commit в `-ldflags` |
| `make run` | запуск без сборки |
| `make test` / `make test-race` | тесты; `-race` — как в CI |
| `make cover` | покрытие в `cover.out` |
| `make coverage` | тесты с `-race` и покрытием + ratchet: общий процент и каждый пакет не ниже `test/coverage-baseline.txt` |
| `make coverage-baseline` | поднять baseline покрытия; понижение - только правкой файла в PR |
| `make gates-test` | тесты ratchet-гейтов (coverage, perf, Complement) на искусственно ухудшенных данных |
| `make perf` / `make perf-baseline` | perf-ratchet `/sync` на бенч-базе (`PSQL_DSN`) / снять baseline на этом железе |
| `make load-smoke` / `make load PROFILE=M` | нагрузка сервера целиком по сети (`cmd/loadgen`, стенд `scripts/load-stand.sh`), профили S/M/L, сравнение с Synapse - [docs/load-testing.md](docs/load-testing.md) |
| `make lint` / `make vet` | golangci-lint v2 и go vet |
| `make vuln` | `govulncheck` по зависимостям и stdlib |
| `make check` | fmt + vet + coverage (test-race + ratchet) + gates-test + lint + vuln (полный прогон перед PR) |
| `make migrate PSQL_DSN=...` | применить миграции (golang-migrate) |
| `make migrate-down PSQL_DSN=... N=1 [YES=1]` | откатить N шагов (N обязателен; база без `test` в имени - только с `YES=1`) |
| `make migrate-version PSQL_DSN=...` | текущая версия схемы |
| `make test-integration SYNGOD_TEST_DSN=...` | интеграционные тесты на живой БД (имя базы обязано содержать `test`) |
| `make docker` | образ `syngo:dev` (multi-stage, distroless) |

## Проверка через Docker

```bash
make docker
docker run --rm -p 8008:8008 -e SYNGOD_ENV=dev -e SYNGOD_MATRIX_SERVER_NAME=localhost syngo:dev
curl -s localhost:8008/health
```

## Структура

```
cmd/syngod/          точка входа: конфиг, логгер, wiring, graceful shutdown
internal/config/     загрузка и валидация конфигурации
internal/api/        HTTP: роутер, middleware, обработчики CS-API
internal/core/       домен: сущности и инварианты (без HTTP и SQL)
internal/store/      PostgreSQL: пул соединений, репозитории
internal/build/      версия/коммит, подставляемые линкером
migrations/          SQL-миграции (up/down)
docs/adr/            архитектурные решения
```

Правило зависимостей: `api → core ← store`; домен ничего не знает о транспорте
и хранилище. Подробности — `docs/adr/0001-architecture.md`.

## Схема БД и миграции

Схема (таблицы, индексы, соответствие таблицам Synapse) — `migrations/0001_init.up.sql` и
`docs/adr/0003-db-schema.md`. Миграции в формате golang-migrate (`NNNN_имя.up.sql` / `.down.sql`),
утилита — `cmd/migrate` (`-up`, `-down N [-yes]`, `-version`, `-force N`). Миграции встроены в
бинари (`migrations/migrations.go`): `syngo-migrate` работает без каталога (`-path` - свой набор).

Защита от разрушительных команд:

- `syngod` при старте сверяет версию схемы (`schema_migrations`) с последней встроенной миграцией
  и не стартует на непримененной, отставшей, более новой или dirty-схеме;
- `-down` без числа шагов - ошибка; на базе без `test` в имени `-down N` требует `-yes`;
- интеграционные тесты (`SYNGOD_TEST_DSN`) и `tools/migration-acceptance/run.sh` работают только с
  базой, имя которой содержит `test`; `cmd/syncload` берет DSN из `SYNGO_SYNCLOAD_DSN`/`-dsn` и
  пишет только в базу с `test`/`bench`/`perf` в имени (`-allow-any-db` - осознанно);
- `federation-keygen` не перезаписывает существующий ключ без `-force`, печатает ключ в stdout
  только по `-out -`.

```bash
export PSQL_DSN='postgres://syngo:syngo@127.0.0.1:5432/syngo?sslmode=disable'
make migrate                 # накатить
make migrate-version         # версия схемы
make migrate-down N=1 YES=1  # откатить одну (база syngo не тестовая - нужно подтверждение)
SYNGOD_TEST_DSN='postgres://syngo:syngo@127.0.0.1:5432/syngo_test?sslmode=disable' make test-integration
```

Доступ к данным — `internal/store` (pgx, явные репозитории): `AppendEvent`, `RoomEventsSince`,
`UpsertRoomState`, `RoomState`; домен зависит от интерфейса `store.EventStore`, а не от pgx.

## Аутентификация

Реализованы `POST /_matrix/client/v3/register`, `POST /login` (`m.login.password`), `POST /logout`,
`POST /logout/all`, `GET /account/whoami`. Токен передаётся заголовком `Authorization: Bearer <token>`
или параметром `?access_token=` (так ходят некоторые клиенты). В БД лежат только SHA-256 от
access-токена и PHC-строка argon2id от пароля — открытые значения существуют только в ответе клиенту.

```bash
export PSQL_DSN='postgres://syngo:syngo@127.0.0.1:5432/syngo?sslmode=disable'
make migrate
SYNGOD_DATABASE_DSN=$PSQL_DSN make run      # второй терминал: сервис на :8008

# 1) первый шаг: сервер отдаёт 401 с flows и session (UIA как в Synapse)
curl -s -X POST localhost:8008/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"correct horse battery"}'
# {"flows":[{"stages":["m.login.dummy"]}],"params":{},"session":"<session>"}

# 2) повтор с auth — регистрация завершается
curl -s -X POST localhost:8008/_matrix/client/v3/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"correct horse battery","auth":{"type":"m.login.dummy","session":"<session>"}}'
# {"user_id":"@alice:localhost","access_token":"syt_...","device_id":"KJHGFRTYUI","home_server":"localhost"}

curl -s -X POST localhost:8008/_matrix/client/v3/login \
  -H 'Content-Type: application/json' \
  -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},"password":"correct horse battery"}'

curl -s localhost:8008/_matrix/client/v3/account/whoami -H "Authorization: Bearer $TOKEN"
# {"user_id":"@alice:localhost","device_id":"KJHGFRTYUI","is_guest":false}

curl -s -X POST localhost:8008/_matrix/client/v3/logout -H "Authorization: Bearer $TOKEN"   # {}
```

Коды ошибок соответствуют спеке: нет токена — `401 M_MISSING_TOKEN`, неизвестный/отозванный —
`401 M_UNKNOWN_TOKEN`, неверный пароль — `403 M_FORBIDDEN`, занятый localpart — `400 M_USER_IN_USE`,
плохой localpart — `400 M_INVALID_USERNAME`, короткий пароль — `400 M_WEAK_PASSWORD`,
закрытая регистрация — `403 M_FORBIDDEN`, деактивированный пользователь — `403 M_USER_DEACTIVATED`.

Параметры — секция `auth` в конфиге: `registration_open` (по умолчанию `false`),
`registration_shared_secret`, `min_password_length`, `refresh_tokens`, `access_token_ttl`. Без `database.dsn` эндпоинты отвечают `503` (авторизация не может работать без БД).

Регистрация идёт через UIA-флоу как в Synapse: первый `POST /register` без `auth` возвращает
`401` с `flows: [{"stages":["m.login.dummy"]}]` и `session`, повторный запрос с
`auth: {"type":"m.login.dummy","session":"…"}` создаёт пользователя. Сессии UIA живут в памяти
процесса (TTL 24 ч); при переходе к нескольким инстансам переедут в БД (план - ADR-0009).

При закрытой регистрации пользователей создает оператор: `GET /_synapse/admin/v1/register` выдает
одноразовый nonce (60 с), `POST` с `mac = hex(HMAC-SHA1(secret, nonce\0user\0password\0admin|notadmin))`
создает пользователя (`admin: true` - администратора). Протокол как в Synapse, работает
`register_new_matrix_user`; без `registration_shared_secret` эндпоинт отвечает `400`.

Не реализовано: реальные UIA-провайдеры (капча, registration token, 3PID),
ротация refresh-токенов через `/refresh`, rate limiting логина.

## Rate limiting

Лимитер устроен как в Synapse («leaky bucket as meter»): на каждый ключ — ведро
со скоростью «протечки» `per_second` и ёмкостью `burst`. Ключ: для логина и
регистрации — IP клиента, для неудачных входов — учетная запись (`alice`, `Alice`,
`@alice:<server_name>` — одно ведро), для отправки событий и загрузки медиа — пользователь,
для `/sync` — устройство пользователя. При превышении сервер отвечает `429` с
`{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":N}` и заголовком `Retry-After` (секунды).

Значения по умолчанию повторяют Synapse (`synapse/config/ratelimiting.py`):

| Ключ конфига | Что ограничивает | per_second | burst | Источник |
|---|---|---|---|---|
| `rate_limit.login_ip` | `POST /login` по IP | 0.003 | 5 | `rc_login.address` |
| `rate_limit.login_account` | неудачные `POST /login` и `POST /_syngo/admin/v1/login` по учетке | 0.003 | 5 | `rc_login.account` |
| `rate_limit.register_ip` | `POST /register` по IP | 0.17 | 3 | `rc_registration` |
| `rate_limit.send` | `PUT …/send/{type}/{txnId}` по пользователю | 0.2 | 10 | `rc_message` |
| `rate_limit.media_upload` | `POST …/media/upload` и `POST /_matrix/media/v1/create` по пользователю | 10 | 50 | `rc_media_create` |
| `rate_limit.sync` | `GET /sync` по устройству (long-poll ждет, а не получает 429) | 2 | 20 | наше решение |

```yaml
rate_limit:
  login_ip:      { enabled: true, per_second: 0.003, burst: 5 }
  login_account: { enabled: true, per_second: 0.003, burst: 5 }
  register_ip:   { enabled: true, per_second: 0.17,  burst: 3 }
  send:          { enabled: true, per_second: 0.2,   burst: 10 }
  media_upload:  { enabled: true, per_second: 10,    burst: 50 }
  sync:          { enabled: true, per_second: 2,     burst: 20 }
```

Выключить лимит целиком — `{ enabled: false }` (или нулевые `per_second`/`burst`);
отключить все сразу — пустой блок `rate_limit: {}` (используется в тестах).

Где лимиты применяются: `login_ip` — на `POST /login` и `POST /_syngo/admin/v1/login`,
`register_ip` — на `POST /register` и `POST /_synapse/admin/v1/register`, `login_account` — на
неудачные попытки входа по одной учетке (ведро расходуется только при `M_FORBIDDEN`, как
`rc_login.account`; CS-логин и admin-логин делят ведро), `send` — на `PUT …/send/…`,
`media_upload` — на upload и `create`, `sync` — на `GET /sync`.

`/sync`: лимит против частого опроса без ожидания (`timeout=0` в цикле). Long-poll
(`timeout > 0`) сверх лимита не отклоняется, а выдерживается до освобождения ведра и затем
выполняется: Element при частых событиях будит long-poll чаще лимита и получает события одной
порцией чуть позже, а не ошибку соединения. 429 — только если ждать пришлось бы дольше `timeout`.

### IP клиента за прокси

Прод-схема ставит TLS-прокси (nginx, traefik) перед сервисом: адрес соединения у всех клиентов —
адрес прокси, и лимиты по IP без настройки общие на всех (пять логинов любых пользователей —
и вход закрыт всем). IP клиента берется из заголовков прокси только для соединений из
`server.trusted_proxies` (CIDR или IP; переменная `SYNGOD_TRUSTED_PROXIES` через запятую):

- `X-Forwarded-For` — справа налево, пропуская доверенные прокси: клиент — первый недоверенный
  адрес (левую часть списка пишет сам клиент, ей не доверяем);
- без `X-Forwarded-For` — `X-Real-IP`;
- соединение не от доверенного прокси — адрес соединения, заголовки игнорируются (подделка
  `X-Forwarded-For` лимит не обходит).

Этот IP используется в лимитах, в логе запросов (`remote_addr`; адрес прокси — `proxy_addr`) и в
аудите admin (`details.ip`). Пусто (по умолчанию) — заголовки не читаются. Сеть `/0` запрещена
валидацией. Отказы логируются как `api: rate limited` с полями `limiter`, `path`,
`retry_after_ms` (IP и user id в этой записи нет).

```yaml
server:
  # nginx на том же хосте, syngod в docker compose: соединения приходят со шлюза
  # сети compose (docker network inspect <проект>_default)
  trusted_proxies: ["172.18.0.1"]
```

```nginx
location / {
    proxy_pass http://127.0.0.1:8008;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Real-IP $remote_addr;
}
```

## Комнаты и сообщения

Первый вертикальный срез Matrix: создание комнаты с genesis-состоянием, отправка событий с
идемпотентностью по `txn_id`, чтение истории с пагинацией.

| Эндпоинт | Что делает |
|---|---|
| `POST /_matrix/client/v3/createRoom` | `room_id`, `m.room.create`, `m.room.member` (join создателя), `m.room.power_levels`, опционально `name`/`topic`/`invite`/`initial_state`; версии комнат 10, 11 и 12 (v12: room_id - хеш `m.room.create` без домена, создатели вне `power_levels`) |
| `PUT /rooms/{roomId}/send/{eventType}/{txnId}` | запись события: `prev_events` — последние события комнаты, `depth` = max+1, `origin_server_ts`, `event_id` = `$` + base64url(sha256(canonical JSON)); повтор с тем же `txn_id` возвращает тот же `event_id` |
| `GET /rooms/{roomId}/event/{eventId}` | событие по id (только участнику) |
| `GET /rooms/{roomId}/messages?from=&dir=b\|f&limit=` | история: курсор — `stream_ordering`, `dir=b` — от новых к старым, `dir=f` — наоборот, `start`/`end` в ответе |
| `POST /rooms/{roomId}/join`, `/invite`, `/leave` | membership-переходы (`m.room.member`) |

Права: писать и читать может только участник комнаты (`membership = join`), иначе `403 M_FORBIDDEN`;
неизвестная комната — `403`, неизвестное событие — `404 M_NOT_FOUND`; версия комнаты вне v10-v12 —
`400 M_UNSUPPORTED_ROOM_VERSION`. `content` нормализуется в компактный UTF-8 JSON перед записью
(как canonical JSON в Synapse).

```bash
# после createRoom (нужен TOKEN и ROOM_ID)
curl -s -X PUT "localhost:8008/_matrix/client/v3/rooms/$ROOM_ID/send/m.room.message/txn1" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"msgtype":"m.text","body":"привет"}'
curl -s "localhost:8008/_matrix/client/v3/rooms/$ROOM_ID/messages?dir=b&limit=10" \
  -H "Authorization: Bearer $TOKEN"
```

Не вошло в этот срез: enforcement power levels (кроме членства), `PUT /rooms/{id}/state`,
`/redact`, `/sync`, alias-эндпоинты, `room_memberships`-история (храним актуальное значение,
история — в `events`; миграция `0003_membership_unique`).

## /sync

Модель, правила доступа к данным и SLO — ADR-0005 (`docs/adr/0005-sync-design.md`).
Реализовано: `GET /_matrix/client/v3/sync` (initial и incremental), long-poll, `lazy_load_members`,
токены позиции с HMAC, стенд и SLO.

| Часть | Где |
|---|---|
| токены `next_batch`/`since`: `v1.e<stream_ordering>.<hmac>` | `internal/core/synctoken.go` |
| сборка данных 3 фиксированными запросами (без N+1) | `core.SyncDataLoader` + `store.SyncStore` |
| HTTP-обработчик, long-poll, lazy-loading, invite/leave | `internal/api/sync.go` |
| стенд и отчет | `cmd/syncload`, `make bench-sync`, perf-ratchet `make perf` |
| гейт числа SQL-запросов | `TestSyncLoaderQueryCountIsConstant` (слой данных), `TestSyncHTTPQueryCountIsConstant` (HTTP) |

Параметры: `since` (пустой — initial), `timeout` (ограничен `sync.max_timeout_ms`), `filter`
(inline JSON, поддержан `room.state.lazy_load_members` и `room.timeline.limit`), `full_state`/`set_presence`
игнорируются как неизвестные параметры. Невалидный токен → `400 M_UNKNOWN_POS`.
Long-poll ждёт событие через in-process нотифаер, **не удерживая соединение с БД**; новый токен в ответе —
текущая `stream_ordering`. Внешние события (другие инстансы) нотифаер не видит — при масштабировании
переедет на `LISTEN/NOTIFY` (ADR-0009); до этого второй экземпляр на той же базе не стартует.

Секрет HMAC: `sync.token_secret` (или `SYNGOD_SYNC_TOKEN_SECRET`). Секрета по умолчанию нет.

| Окружение (`env` / `SYNGOD_ENV`) | Пустой секрет |
|---|---|
| `dev`, `test` | сервер стартует со случайным секретом и пишет WARN в лог; токены не выживают рестарт и не работают между инстансами |
| `prod` или не задано | **отказ старта** с сообщением «config: sync.token_secret is required outside dev/test»; секрет короче 32 символов - тоже отказ |

```bash
# локальная разработка: случайный секрет, в лог уйдёт WARN
SYNGOD_ENV=dev make run
# прод: явный секрет обязателен
SYNGOD_ENV=prod SYNGOD_SYNC_TOKEN_SECRET="$(openssl rand -hex 32)" ./bin/syngod
```

### Стенд `make bench-sync`

Генерирует профиль (комнаты × события × участники) с состоянием комнат (create, power_levels,
name, member-события в `room_state`) и меряет два уровня:

- слой данных `/sync` (`core.SyncDataLoader`): сценарии `initial`, `empty`, `delta`;
- HTTP-обработчик `GET /sync` целиком (токен, middleware, account data, to-device, ключи, JSON) в том
  же процессе, без сети: `http-initial`, `http-empty`, `http-delta`.

Метрики: p50/p95/p99, RPS, число SQL-запросов на одну сборку (pgx-трейсер, счетчик в контексте - не
смешивается между клиентами), число замеров и ошибок, RSS процесса и железо (модель CPU, ядра, RAM,
load average, версии Go и PostgreSQL). Ошибка сборки `/sync` - ненулевой код выхода.
Отчет - `bench/sync-<rooms>-rooms.md`.

```bash
PSQL_DSN='postgres://…/syngo_bench' make migrate   # отдельная бенч-база: имя с test/bench/perf
PSQL_DSN='postgres://…' make bench-sync              # 1000 комнат, 20 событий, 10 клиентов
ROOMS=1000 CLIENTS=50 PSQL_DSN='…' make bench-sync   # профиль из ADR: empty на 50 клиентах
```

Цифры имеют смысл только вместе с железом: прежний эталон (1000 × 20 × 50, initial p99 1011 мс) снят
без записи хоста и на другой машине не воспроизвелся (2681-2738 мс). Текущий baseline perf-ratchet
и железо, на котором он снят, - в `bench/sync-baseline.json`.

Нагрузка сервера целиком (сеть, все эндпоинты клиента, задержка доставки сообщения
отправитель -> /sync получателя) - `cmd/loadgen`, [docs/load-testing.md](docs/load-testing.md).

### Гейты производительности /sync

- **Число SQL-запросов** (ADR-0005 §2: не больше 8 на слой данных и не растет с числом комнат) -
  интеграционные тесты, CI-джоба `integration`: `TestSyncLoaderQueryCountIsConstant` (10 и 100 комнат,
  initial и incremental) и `TestSyncHTTPQueryCountIsConstant` (HTTP `/sync`, 3 и 15 комнат, бюджет 20,
  замер 16). Трейсер подключен к пулу, счетчик 0 - падение теста.
- **perf-ratchet** - `scripts/perf-ratchet.sh`, правила в `tools/ratchet/perf_ratchet.py`: профиль
  прогона берется из baseline; регрессия - рост SQL-запросов в любом сценарии, p99 выше
  `max(baseline × 1.25, baseline + 10 мс)`, ошибки или 0 замеров. Время сравнивается только на том же
  железе (модель CPU и ядра), иначе отказ с просьбой снять baseline на этой машине
  (`make perf-baseline`). Пороги - константы, окружением не ослабляются.
  База - чистая схема: число запросов зависит от формы данных (`-require-empty`).
- Текущий baseline снят 20.11.2026 на машине разработки (Xeon E5-2620 v4, 8 CPU, load 8-50 от
  соседних процессов): повторный прогон там давал p99 HTTP-сценариев до +50%, поэтому время
  сравнивается только с baseline, снятым на той же машине. SQL-запросы стабильны и сравниваются
  всегда. В CI perf-ratchet не запускается: baseline привязан к железу.

## Профиль и устройства

| Эндпоинт | Что делает |
|---|---|
| `GET /profile/{userId}` | `displayname` + `avatar_url`; чужой профиль виден только при общей комнате, иначе `404 M_NOT_FOUND` |
| `GET/PUT /profile/{userId}/displayname`, `.../avatar_url` | PUT — только свой профиль (иначе `403 M_FORBIDDEN`) |
| `GET /devices`, `GET/PUT/DELETE /devices/{deviceId}` | список устройств, переименование, удаление |
| удаление устройства | одной транзакцией стирает `access_tokens`, `device_keys`, `one_time_keys` и само устройство — токен устройства перестаёт работать |

Приватность профиля — как в Synapse: без общей комнаты (`room_memberships`, обе стороны `join`)
ответ `404`, чтобы не подтверждать существование пользователя. `POST /delete_devices` (UIA) в этот
срез не входит.

## Миграция с Synapse

Путь миграции, что переносится и по какой цене — ADR-0006 (`docs/adr/0006-migration-path.md`).

Коротко: у Synapse нет полного экспортного API, поэтому дамп собирается с read-only реплики БД в
**JSONL** (`tools/synapse-export/export.sql`), медиа копируется из `media_store`
(`local_content/<2>/<2>/<rest>`). Импорт — `cmd/syngo-import`, идемпотентный, сохраняет `event_id`,
`stream_ordering`, `depth` и состояние комнат. Обратный путь (exit path) — тот же JSONL, формируемый
нашими таблицами; описан в ADR-0006 §7.

```bash
# 1) экспорт из Synapse (replica, read-only)
psql "$SYNAPSE_DSN" -At -f tools/synapse-export/export.sql > synapse-dump.jsonl

# 2) импорт в чистую схему SYNGO
PSQL_DSN='postgres://…/syngo_bench' make migrate   # отдельная бенч-база: имя с test/bench/perf
go run ./cmd/syngo-import -dsn "$PSQL_DSN" -file synapse-dump.jsonl -server-name example.org

# 3) проверка: история комнаты читается через обычный API
curl -s "localhost:8008/_matrix/client/v3/rooms/!room:example.org/messages?dir=b" \
  -H "Authorization: Bearer $TOKEN"
```

`-dry-run` считает записи без записи в БД, `-limit N` ограничивает объём (отладка),
`-report import-report.json` пишет отчёт (счётчики по типам, ошибки на строку, длительность),
`-batch N` задаёт шаг прогресса в логе. Ошибка на строке не останавливает импорт — она попадает
в отчёт, процесс продолжается.

Кроме событий переносятся устройства и токены, E2EE-ключи, account data, push-правила, pushers и
фильтры (`syngo-import`); не переносятся эфемерные данные (receipts, presence, typing) и кеши
(превью ссылок, миниатюры, чужие медиа). Полный список и порядок шагов - `docs/migration-runbook.md`.
Пароли: у Synapse bcrypt, у нас argon2id - bcrypt-хеши принимаются при входе и переводятся в argon2id
(раздел «Миграция с Synapse: пароли»).

## Complement и ratchet

Готовность меряем официальным сьютом Matrix — [Complement](https://github.com/matrix-org/complement).
Прогон: три команды, Docker и Go 1.27+.

```bash
make test-complement        # собрать образ, прогнать Complement, проверить ratchet
make complement-baseline    # принять текущий результат как baseline (осознанно!)
make complement-ratchet-test  # тесты ratchet без Docker
```

Как это устроено:

- `Dockerfile` (target `complement`) — самодостаточный образ: внутри стартует PostgreSQL,
  накатываются миграции и запускается `syngod` на 8008. Complement передаёт только `SERVER_NAME`.
- `scripts/complement.sh` — сборка образа, прогон Complement зафиксированной версии
  (`COMPLEMENT_REF`) с `go test -json`, нормализация и проверка ratchet.
- `test/complement/results.jsonl` — версионируемый baseline: пары `{Action, Test}`, отсортированы.
- `test/complement/allowlist.txt` — исключения `<тест> | <причина> | <дата пересмотра>`;
  исключение без причины или с просроченной датой валит проверку.
- `tools/complement/complement_ratchet.py` — нормализация и сверка: тест, который проходил,
  обязан проходить и дальше (fail/skip/отсутствие - регрессия); pass не меньше, чем в baseline;
  набор тестов не сокращается; прогон без единого теста - падение.
- CI: workflow `.github/workflows/complement.yml` с ручным запуском (workflow_dispatch); артефакты -
  `complement-results.jsonl` (нормализованный прогон) и лог `go test`.

В прогоне 25 ноября 2026 года прошли 340 из 553 зарегистрированных проверок (61,5%),
включая подтесты. На верхнем уровне прошли 88 из 142 зарегистрированных тестов. В пакете
клиентского API прошли 323 проверки, завершилась ошибкой 61, пропущена одна. В набор входят
проверки устаревших и экспериментальных версий комнат. syngo поддерживает версии 10–12.
В списке исключений остаются `TestFilter` и `TestContent`.

Прогон идет с `COMPLEMENT_ENABLE_DIRTY_RUNS=1` (один homeserver на пакет, как у Synapse) и
таймаутом `COMPLEMENT_GO_TEST_TIMEOUT` (35m): без dirty runs каждый тест поднимал контейнер из
снимка blueprint, и csapi не укладывался в таймаут.

Обновление baseline - только через проверку: `make complement-baseline` и
`scripts/complement.sh --adopt-results=<артефакт>` пишут baseline, лишь если регрессий нет и набор
тестов не сократился; `normalize` в baseline не пишет. Дата снятия - в заголовке файла
(`# measured:`), allowlist проверяется на сегодняшнюю дату. Сократить набор (например, после смены
`COMPLEMENT_REF`) можно только правкой файла в PR.

Complement не публикует теги — версия пинуется коммитом в `COMPLEMENT_REF`
(`scripts/complement.sh`), порядок обновления описан в `test/complement/README.md`.

## Медиа-репозиторий

Эндпоинты (CS API v1.11: аутентифицированное медиа MSC3916, асинхронная загрузка MSC2246):

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/_matrix/media/v3/upload` (алиас `/_matrix/client/v1/media/upload`) | загрузка (auth + rate limit `media_upload`) |
| `POST` | `/_matrix/media/v1/create` | placeholder с серверным `media_id` и `unused_expires_at` (auth + `media_upload`, не больше `max_pending_uploads` незаполненных) |
| `PUT` | `/_matrix/media/v3/upload/{serverName}/{mediaId}` | байты в placeholder: `403 M_FORBIDDEN` чужой, `409 M_CANNOT_OVERWRITE_MEDIA` повтор, `404` просрочен, `413 M_TOO_LARGE` |
| `GET` | `/_matrix/client/v1/media/download/{serverName}/{mediaId}[/{fileName}]` | отдача байтов (auth); `timeout_ms` ждет async-загрузку (20 с, не больше 60 с), затем `504 M_NOT_YET_UPLOADED` |
| `GET` | `/_matrix/client/v1/media/thumbnail/{serverName}/{mediaId}` | миниатюра `method=scale\|crop` (auth), PNG для PNG/GIF, иначе JPEG |
| `GET` | `/_matrix/client/v1/media/config`, `/_matrix/media/v3/config` | `{"m.upload.size": N}` (auth; без лимита поле не отдается) |
| `GET` | `…/preview_url` | пустой объект `{}` (auth; превью ссылок не генерируется) |
| `GET` | `/_matrix/media/v3/download\|thumbnail/...` | легаси без токена: только медиа, созданные до `legacy_freeze` |

Отдача: `Content-Disposition: inline` только для безопасных типов (список Synapse), остальное -
`attachment`; `Content-Security-Policy: sandbox; default-src 'none'; …`, `X-Content-Type-Options: nosniff`,
`Cross-Origin-Resource-Policy: cross-origin`.

Конфиг:

```yaml
media:
  storage_backend: local     # local (реализовано); s3 — ошибка валидации, есть интерфейс ObjectStore
  directory: var/media       # локальный каталог, env SYNGOD_MEDIA_DIRECTORY
  max_upload_bytes: 52428800 # 50 MiB; превышение → 413 M_TOO_LARGE
  max_pending_uploads: 5     # незаполненных placeholder на пользователя
  unused_expiration: 24h     # срок заполнения placeholder
  enable_authenticated_media: true        # false - легаси-пути отдают все медиа
  legacy_freeze: "2026-11-20T00:00:00Z"   # медиа позже - только через v1 с токеном
```

Устройство: `internal/media.Service` (генерация `mxc://<server>/<id>`, sha256, лимит, имена файлов)
+ `ObjectStore` (локальный каталог; S3/MinIO — вторая реализация интерфейса) + метаданные в таблице
`media` (миграция `0002_media`). Ключ хранения генерируется сервером (`<server>/<media_id>`, для
async-загрузки - с суффиксом попытки), путь из имени файла не используется - `filename` идет только
в `Content-Disposition` и очищается от пути.

## Миграция с Synapse: пароли (bcrypt → argon2id)

Synapse хранит пароли в **bcrypt**, мы — в **argon2id**. Миграция схемы не нужна: формат определяется
по префиксу строки в `password_hash`.

- При входе `internal/auth.MultiHasher` выбирает верификатор: `$argon2id$…` → argon2id,
  `$2a$/$2b$/$2y$…` → bcrypt (с ограничением cost ≤ 15, чтобы подделанный хеш не стал DoS).
- После успешного входа bcrypt-хеш **прозрачно пере-хешируется** в argon2id (`core.Auth.rehashPassword`);
  если запись не удалась — вход всё равно проходит, попытка повторится в следующий раз.
- Метрика перехода: при старте в лог пишется `password hashes argon2id=N bcrypt_legacy=M`,
  каждый перенос — `auth: password rehashed to argon2id user_id=…`.
- Деактивированные пользователи (`deactivated`) по-прежнему получают `M_USER_DEACTIVATED` на логине.

Импорт пользователей (PoC `cmd/syngo-import`, ADR-0006 §4) копирует `user_id`, `display_name`,
`deactivated` и `password_hash` **как есть** — то есть bcrypt-хеши попадают в базу без изменений
и переходят на argon2id при первом входе. Повторный импорт идемпотентен (`ON CONFLICT DO NOTHING`).

```bash
# импорт дампа (JSONL, см. tools/synapse-export/export.sql)
syngo-import -dsn "$PSQL_DSN" -file synapse-dump.jsonl -server-name matrix.example.org
# после импорта: сколько ещё на bcrypt
grep 'password hashes' /var/log/syngod.log | tail -1
```

## Миграция с Synapse: медиа (файлы + метаданные)

Медиа переносится отдельной командой `syngo-import-media`, потому что байты лежат в
`media_store`, а метаданные — в БД Synapse.

```bash
# 1) на стороне Synapse: экспорт метаданных (kind=media) — tools/synapse-export/export.sql
# 2) у нас: файлы + метаданные
syngo-import-media \
  -dsn "$PSQL_DSN" \
  -dump media.jsonl \
  -media-store /var/lib/matrix-synapse/media_store \
  -object-dir var/media \
  -server-name matrix.example.org \
  -report media-import.txt
```

Что делает:

- раскладку Synapse `local_content/<2>/<2>/<rest>` сохраняет как `storage_key` (совместимо с тем,
  что записал `syngo-import`), копирует байты в наш `ObjectStore` (`-workers`, по умолчанию 4 потока),
  считает **sha256** и сверяет его с `local_media_repository.sha256` Synapse (`hash_mismatch`), размер -
  с `media_length` (`mismatch`); в метаданные пишется размер и sha256 **реально скопированных байтов**
  (Content-Length скачивания берется из БД);
- карантин (`quarantined_by`) переносится и соблюдается: медиа заблокировано, download и миниатюры - 404;
- превью ссылок (строки с `url_cache`) не переносятся - это кеш, syngod запросит превью заново; в отчете
  у них статус `url_cache`, ошибкой не считаются;
- миниатюры не переносятся - строятся при первом запросе и кешируются; `remote_media_cache`
  (чужие медиа) не переносится - федерация вне скоупа;
- отсутствующие файлы → `missing`, **метаданные не теряются** (download вернёт 404);
- повторный прогон идемпотентен и **ничего не копирует**: медиа, у которого в БД sha256 и тот же размер в
  хранилище (и sha256 совпадает с Synapse), - `skipped`, метаданные (карантин) обновляются; так же
  продолжается прогон после обрыва, и так же медиа можно перенести заранее, а в окне простоя
  докопировать разницу; `-rehash` - перечитать объект и сверить sha256 перед пропуском;
- отчёт (`-report` и stdout): `copied/skipped/url_cache/missing/mismatch/hash_mismatch/failed/quarantined`,
  затем строки `problem:` с расхождениями, затем строка на каждое медиа; при `missing`, `mismatch`,
  `hash_mismatch`, `failed` код возврата ненулевой. Сверка с БД Synapse - `syngo-verify-migration`
  (медиа: набор, размер, карантин, sha256).

## Миграция с Synapse: история событий

История событий — самая объёмная часть дампа, поэтому импортируется отдельной командой
`syngo-import-events` потоком, пачками, с сохранением `event_id`, `stream_ordering`,
`depth`, `prev_events`, `auth_events` и `origin_server_ts`.

```bash
# 1) на Synapse: все события одним снимком, с манифестом в конце
syngo-synapse-export -synapse-dsn "$SYNAPSE_DSN" -out events.jsonl

# 2) у нас (комнаты уже импортированы через syngo-import)
syngo-import-events -dsn "$PSQL_DSN" -file events.jsonl -batch 1000 -report events-import.txt \
  -checkpoint events.checkpoint -bad-file events-bad.jsonl
# можно из stdin:  zstd -dc events.jsonl.zst | syngo-import-events -dsn "$PSQL_DSN" -file -

# 3) сверка с Synapse: число, границы stream_ordering, комнаты, event_id
syngo-verify-migration -synapse-dsn "$SYNAPSE_DSN" -dsn "$PSQL_DSN" -report verify.txt
```

Свойства:

- **стриминг**: дамп читается построчно, память ограничена размером пачки (`-batch`, по умолчанию 1000);
- **идемпотентность**: `INSERT … ON CONFLICT (event_id) DO NOTHING`, повторный прогон не дублирует;
- **целостность**: события неизвестных комнат и битые строки не роняют импорт — идут в отчёт (`bad`),
  счётчики по типам событий, до 20 первых ошибок с номерами строк, сами строки - в `-bad-file`;
  пачка, упавшая на ошибке данных одной строки, повторяется по одному событию; код возврата
  ненулевой, если `bad > 0` или число событий не совпало с манифестом выгрузки;
- **чекпойнт**: `-checkpoint FILE` - после каждой пачки пишется номер записанной строки, повторный
  запуск продолжает с нее (без флага повтор идет с начала, идемпотентно);
- **форматы ссылок**: `prev_events`/`auth_events` комнат v1/v2 (пары `[id, {sha256}]`) разбираются;
  `\u0000` в `content` (jsonb его не принимает) заменяется на U+FFFD;
- **backfill**: события с отрицательным `stream_ordering` сохраняют его и стоят в начале истории;
- **прогресс**: каждые N событий (`-progress`, по умолчанию 50 000) — `read/inserted/skipped/bad`, rate, elapsed;
- **отчёт**: `-report` (и stdout) с длительностью и RSS процесса — годится для приёмки и замера;
- **состояние комнат**: строки `kind=state` (`current_state_events` Synapse) заменяют `room_state` комнаты
  как есть; для дампов старого формата без них в конце импорта `room_state` материализуется
  эвристикой (последнее state-событие по `(type, state_key)`), отдельная команда -
  `syngo-recompute-state -dsn "$PSQL_DSN"`;
- **исходный PDU**: `events.pdu_json` (текст `event_json.json`), `signatures`, `hashes`, `origin` из PDU,
  `event_auth` по `auth_events`; `event_id` сверяется с reference hash PDU (комнаты v3+), расхождение -
  ошибка импорта;
- **отклоненные, soft-failed, outlier** - в `excluded_events`: сохранены с PDU, но ни timeline, ни состояние,
  ни поиск их не видят;
- **redactions**: `m.room.redaction` записывается в `redactions` и применяется в конце импорта (цель может
  идти позже): `redacted_by`, вырезанные `content` и `pdu_json`, оригинал - в `redactions.original_content`
  с временем redaction; `-redaction-retention` (по умолчанию 7 дней, как в Synapse) - хранить ли его,
  стирает syngod по `redaction.retention_period`;
- **последовательность**: в конце импорта выполняется `setval('events_stream_ordering_seq',
  GREATEST(max(stream_ordering), last_value))`, поэтому новый локальный `/send` после импорта получает
  `stream_ordering` больше импортированных, а `since`-токены `/sync` не «уезжают» назад. Повторный
  импорт счётчик не откатывает;
- **события без `stream_ordering`** в дампе получают `nextval` (а не 0) — не оказываются «до» истории.

Замер (локальный PostgreSQL 17, `go test -tags integration -run TestImport100kPerformance`):
**100 000 событий — 3.0 с (~33 000 событий/с), RSS 23.6 MiB**, память не растёт с размером дампа
(генерация дампа в тесте тоже потоковая).

## Миграция: exit path (`syngo-export`)

Обратный путь обязателен (ADR-0006 §7): без него повторяем ошибку Dendrite — «въехать можно, уехать нельзя».
`syngo-export` выгружает наши таблицы в **тот же JSONL-формат**, который читают импортёры, стримингом
по `stream_ordering`:

```bash
syngo-export -dsn "$PSQL_DSN" -out syngo-dump.jsonl \
  -media-dir var/media -media-out /tmp/synapse-media \
  -report export-report.txt
```

Формат - тот же, что у `syngo-synapse-export`:

- `kind=user` — `user_id`, `localpart`, `password_hash` (argon2id PHC как есть), `display_name`, `deactivated`;
- `kind=room`, `kind=room_alias`;
- `kind=event` — пачками по `stream_ordering`, включая отрицательные (backfill): метаданные и исходный PDU
  (`pdu_json`) в поле `json`; отклоненные, soft-failed и outlier (`excluded_events`) - с теми же признаками,
  что в дампе Synapse; у удаленного события - оригинальный `content` из `redactions.original_content`
  (пока не истек `redaction.retention_period`): импорт применит redaction заново и сохранит оригинал так же;
- `kind=state` — текущее состояние комнат (`room_state`), затем `kind=manifest`;
- `kind=membership` — текущее членство с `event_id` и `forgotten` (после событий: FK);
- `kind=receipt` — отметки прочтения `m.read` и `m.read.private` локальных пользователей, как в
  `export.sql`;
- `kind=media` — метаданные с `quarantined_by` и `sha256`, файлы копируются в раскладку Synapse
  `local_content/<2>/<2>/<rest>` (совместимо с `media_store` Synapse).

Для отката после cutover (ADR-0006 §7) - только созданное после переключения:
`-after-stream <max stream_ordering импорта>` (события) и `-media-since <время cutover>` (медиа).

Дамп читается нашими же импортёрами (`syngo-import`, `syngo-import-events`, `syngo-import-media`),
формат документирован и не требует ручной правки. Проверено round-trip тестом
(`go test -tags integration -run TestRoundTripSynapseFixture ./cmd/syngo-import/`): фикстура живого
Synapse 1.165 → импорт по runbook → сверка с Synapse → `syngo-export` → импорт в чистую схему → сверка с
тем же Synapse снова `OK`, исходные PDU, оригиналы удаленных событий, карантин и sha256 медиа совпадают.

**Что теряется при переносе в Synapse** (и почему):

| Данные | Причина |
|---|---|
| Пароли | у нас argon2id, Synapse умеет bcrypt → **сброс пароля** при импорте (в дампе хеш как есть, чтобы не терять его при возврате к нам) |
| E2EE-ключи устройств, `to_device` | не экспортируются: клиенты перевыпустят ключи после переезда |
| `account_data`, receipts, presence, typing | эфемерные/клиентские данные, восстанавливаются клиентами (receipts в дампе есть, `kind=receipt`, но загрузчика в Synapse нет) |
| Серверные ключи федерации | генерируются заново на новом сервере |

## To-device сообщения (E2EE-транспорт)

`PUT /_matrix/client/v3/sendToDevice/{eventType}/{txnId}` кладёт сообщение в очередь
`device_inbox` для каждого устройства-получателя; `GET /sync` отдаёт его в `to_device.events`
**один раз** и удаляет из очереди (доставка одноразовая). Идемпотентность — по `txnId`
(таблица `transactions`). To-device не попадает в timeline/DAG.

- `next_batch` учитывает `max(events, device_inbox, device_lists_changes)` — иначе сообщение,
  записанное после последнего события, выпадало бы из окна синхронизации.
- `device_one_time_keys_count` в `/sync` — реальные счётчики из `one_time_keys`;
  `device_lists.changed` — пользователи, чьи ключи устройств менялись после `since`.
- `POST /keys/device_signing/upload` и `POST /keys/signatures/upload` — хранение кросс-подписей
  и подписей устройств (сервер не проверяет криптографию, только раздаёт);
  `GET /keys/changes` — список изменившихся пользователей после `from`.

## Федерация: ключи сервера и подписи

Первый блок федерации (ADR-0007): ключ сервера, публикация `/_matrix/key/v2/server`, подписи
`X-Matrix` и кэш чужих ключей.

```bash
# 1) сгенерировать ключ (формат Synapse: ed25519 <version> <base64-seed>)
go run ./cmd/federation-keygen -version a_1 -out /etc/syngo/federation.key   # существующий не перезапишет без -force

# 2) включить федерацию
export SYNGOD_FEDERATION_ENABLED=1
export SYNGOD_FEDERATION_SIGNING_KEY=/etc/syngo/federation.key
```

- Файл ключа обязан быть `chmod 600` — сервер откажется стартовать, если права шире.
  В БД и логи приватный ключ не попадает: в БД кэшируются только **чужие** публичные ключи.
- Без ключа при `federation.enabled: true` старт невозможен (явная ошибка).
- **Backup обязателен**: потеря ключа = невозможность подписывать запросы. При смене ключа
  прежний публичный ключ задается в `federation.old_signing_keys` (как в Synapse:
  `{"ed25519:id": {key: <base64>, expired_ts: <ms>}}`) и публикуется в `old_verify_keys`.
- Файл ключа Synapse (`signing.key`, seed без паддинга) читается как есть.
- Публикация: `GET /_matrix/key/v2/server` (и `/{keyId}`) отдаёт `server_name`, `valid_until_ts`
  (`federation.valid_until_days`, по умолчанию и не более 7), `verify_keys`, `old_verify_keys` и подпись
  самого ответа нашим ключом (канонический JSON).
- Входящие федеративные запросы проверяются middleware `requireFederation`: заголовок
  `Authorization: X-Matrix origin=…,destination=…,key=…,sig=…`, подпись по (метод, URI, каноническое тело),
  `destination` = наш `server_name`, ключ — из кэша `server_signing_keys` или запросом:
  сначала нотарии `trusted_key_servers` (`POST /_matrix/key/v2/query`, ответ подписан сервером и
  нотарием; формат Synapse - имя или `{server_name, verify_keys}`), затем сам сервер через резолвер
  (well-known/SRV) и защищенный транспорт. До запроса ключей origin проверяется по грамматике
  `server_name` и по `allow_hosts`/`deny_hosts` (403).
- Ответ с ключами: `server_name` совпадает, `valid_until_ts` обязателен и ограничивается 7 днями от
  получения, все подписи ключами из `verify_keys` верны; `old_verify_keys` кэшируются до `expired_ts`.
- Кэш ключей: таблица `server_signing_keys`; при истечении срока ключ перезапрашивается; если свежего
  нет, используется прежний: срок ключа сверяется с `origin_server_ts` события, поэтому старые события
  им проверяются, а запросы (по текущему времени) - нет. Одновременные запросы ключей сервера
  сливаются, неудача запоминается на 30 c, успешный ответ - на 10 c (перебор `key_id` не плодит запросов).

## Федерация: транспорт

Сетевой слой федерации: резолв адреса, подписанные запросы, ретраи, политика хостов и лимиты входящих.

- **Резолв** (`internal/federation/transport.Resolver`) по спеке: IP-литерал и имя с явным портом -
  без well-known/SRV; иначе `/.well-known/matrix/server` (HTTPS, кэш с TTL, отсутствие - 2 мин,
  не более 5 редиректов) → для delegated_hostname те же шаги → SRV `_matrix-fed._tcp`
  (затем `_matrix._tcp`) → A/AAAA на **8448**, при сетевой ошибке — **fallback на 443**
  (важно для серверов за прокси в закрытых контурах). Рабочий порт кэшируется на TTL, чтобы не пробовать
  недоступный 8448 каждый раз. `m.server` проверяется по грамматике `server_name` (путь, query,
  схема, userinfo отвергаются).
- **TLS**: SNI и проверка сертификата - на `server_name` (или delegated_hostname),
  а не на имя цели SRV или IP; заголовок `Host` - по спеке.
- **SSRF**: `federation.ip_range_blacklist` (по умолчанию частные и служебные диапазоны,
  как в Synapse) и `federation.ip_range_whitelist` (исключения); адрес проверяется после резолва
  и перед connect, для API, well-known и ключей. Клиент федерации не следует редиректам, прокси
  из окружения не использует. На стенде (`deploy/stand/config-*.yaml`) docker-сеть и 127/8 в whitelist.
- **Клиент**: connect 5 c, общий таймаут 30 c, пул соединений, `User-Agent`, лимит тела ответа
  (`max_transaction_bytes`), каждый запрос подписывается `X-Matrix` ключом сервера.
- **Retry/backoff**: экспоненциальный с jitter (±20%), старт `retry_min` (5 c), потолок `retry_max` (1 ч);
  состояние хранится в таблице `destinations` (`failure_ts`, `retry_last_ts`, `retry_interval`),
  пока пауза активна — `ErrBackoff` (без сетевого запроса), успех сбрасывает состояние.
- **Политика хостов**: `federation.allow_hosts` (пусто = все) и `federation.deny_hosts`
  (deny важнее allow; совпадение точное или по суффиксу с точкой).
- **Входящие**: `federation.max_transaction_bytes` (413 `M_TOO_LARGE`), rate limit до проверки подписи -
  по IP клиента, после - по проверенному origin (`incoming_rate_per_second`/`incoming_burst`
  → 429 `M_LIMIT_EXCEEDED` + `Retry-After` и `retry_after_ms`); `allow_hosts`/`deny_hosts` действуют
  и на входящие (403). Открыт `GET /_matrix/federation/v1/version` (проверяет подпись).
- **Backoff**: сеть и 5xx - пауза destination; слишком большой ответ (`ErrTooLarge`) - нет; прочий
  ответ сервера сбрасывает паузу.
- **Метрики** без PII: счётчики запросов/ошибок/пропусков по backoff и по destination (`Client.Snapshot()`),
  в логах — destination, класс ошибки, задержка; тела событий не логируются.

```yaml
federation:
  allow_hosts: ["matrix.example.org", "partner.example"]
  deny_hosts: ["blocked.example"]
  ip_range_whitelist: []        # исключения из ip_range_blacklist (по умолчанию - частные сети)
  trusted_key_servers: []       # ["matrix.org"] или [{server_name: matrix.org, verify_keys: {...}}]
  max_transaction_bytes: 1048576
  connect_timeout: 5s
  request_timeout: 30s
  retry_min: 5s
  retry_max: 1h
  incoming_rate_per_second: 10
  incoming_burst: 50
```

## Федерация: очереди outbox/inbox

- **Исходящая очередь** `federation_outbox` (`destination`, `pdu_json`, `queued_at`, `attempts`,
  `next_attempt_at`) + воркер (`internal/federation/outbox`): выбирает батчи по destination,
  отправляет `PUT /_matrix/federation/v1/send/{txnId}` через транспорт федерации, удаляет при 2xx,
  иначе `attempts++` и экспоненциальный backoff (5 с → 1 ч). Один воркер на процесс, стартует из `main`.
- **Входящая очередь/дедуп** `federation_inbox` с `PRIMARY KEY (origin, txn_id)` — **в БД**, переживает
  рестарт: повторный `txnId` возвращает 200 и ничего не применяет. PDU применяются идемпотентно
  по `event_id` (`ON CONFLICT DO NOTHING`), EDUs пока логируются.
- **`destination_rooms`** — какие комнаты отправлять какому серверу; заполняется при приёме PDU от
  origin и при первом событии в комнату (хранение через `store.SetDestinationRoom`).
- **Метрики без PII**: `OutboxMetrics` (глубина, возраст самого старого элемента, сумма попыток)
  логируются воркером; тела событий в логи не попадают.

```bash
# посмотреть очередь
psql "$PSQL_DSN" -c "select destination, count(*), max(attempts) from federation_outbox group by 1"
```

## Федерация: подписи PDU, auth rules и join-флоу

- **Подпись PDU проверяется:** входящее событие обязано содержать `signatures[<домен отправителя>]`,
  подпись считается по каноническому JSON события (без `signatures`) ключом сервера-источника
  из кэша (`signing.VerifyJSON`). Нет подписи/ключ неизвестен/подпись не сходится → `403 M_FORBIDDEN`,
  inbox не помечается, DAG не меняется.
- **Auth rules (v10/v11, минимальный набор):** `m.room.create` (без `prev_events`, домен sender'а = домен
  комнаты), `m.room.member` (join по invite или `join_rule: public`, leave собой, kick/ban по power levels),
  `m.room.power_levels` (только участник с power ≥ 100), прочие события — sender должен быть в комнате,
  для state-событий проверяется `state_default`/`events`. Событие **без `auth_events` отклоняется**.
  v12: создатели (отправитель create и `additional_creators`) с бесконечным уровнем, их
  нельзя вписать в `power_levels.users`, create в `auth_events` запрещен; по федерации комнаты v12
  не принимаются и не отдаются (`M_INCOMPATIBLE_ROOM_VERSION`).
- **`event_auth` (миграция `0009_event_auth`):** при приёме PDU сохраняются ссылки `event_id → auth_event_id`,
  auth-состояние для проверки собирается из этих событий.
- **Join-флоу:** `GET /_matrix/federation/v1/make_join/{roomId}/{userId}` отдаёт шаблон `m.room.member(join)`
  с `prev_events`/`depth`; `PUT /_matrix/federation/v1/send_join/{roomId}/{eventId}` принимает подписанное
  событие, проверяет подпись и auth rules, пишет в DAG, обновляет `room_memberships`/`room_state`
  и `destination_rooms`, отдаёт состояние комнаты в v1-формате `[200, {...}]`.
- Исходящая сторона (наш пользователь входит в чужую комнату через make_join/send_join и outbox) -
  раздел «Федерация: исходящий join».

## Федерация: исходящий join

Наш пользователь входит в комнату на чужом сервере: `POST /rooms/{roomId}/join` для `room_id`
с чужим доменом запускает федеративный флоу (`internal/federation/join`):

1. подписанный `GET /_matrix/federation/v1/make_join/{roomId}/{userId}` через транспорт федерации;
2. заполнение шаблона (`sender`, `state_key`, `room_id`, `origin`, `origin_server_ts`)
   и подпись события ключом нашего сервера;
3. `PUT /_matrix/federation/v1/send_join/{roomId}/{eventId}` — ответ v1 `[200, {state, auth_chain, event}]`;
4. **применение результата одной транзакцией** (`store.ApplyRemoteJoin`): комната, события
   (`events` + `event_auth`), `room_state`, `room_memberships`, `destination_rooms`;
   подписи всех полученных событий проверяются **до** записи — при ошибке транзакция откатывается
   и «полу-вступления» не остаётся.

Ключи чужих серверов для проверки state берутся из `/_matrix/key/v2/server` через тот же транспорт
(в проде — из кэша `server_signing_keys`). Если сервер отклонил join или подпись state не сошлась —
клиент получает `502`, локальное состояние не меняется.

## Федерация: обмен событиями и состоянием

- **Fan-out через outbox:** при локальном `send`/`invite`/`leave` событие подписывается нашим ключом
  и кладётся в `federation_outbox` для каждого destination комнаты (`destination_rooms`).
  Синхронных отправок нет — клиент получает ответ сразу, доставку делает воркер outbox.
- **Чтение (входящие федеративные маршруты, за подписью и лимитами):**
  `GET /event/{eventId}` (событие + auth_chain), `GET /state/{roomId}` и `/state_ids/{roomId}` (v1-формат
  `[200, {pdus, auth_chain}]` / `[200, {pdu_ids, auth_chain_ids}]`),
  `POST /get_missing_events/{roomId}` (закрытие дыр по `earliest_events`/`latest_events`/`limit`/`min_depth`),
  `GET /backfill/{roomId}?v=&limit=` (история по depth для догона при первом `/messages`).
- **Входящее приглашение:** `PUT /invite/{roomId}/{eventId}` — проверка подписи отправителя и запись
  `m.room.member(invite)` в `room_memberships`/`room_state`; `/sync` покажет приглашение.
- Тесты: state/state_ids/event, `get_missing_events` (порядок по depth), `/backfill`,
  входящий invite и fan-out в `outbox` (подписанный PDU с телом сообщения).

## Федерация: автодогон дыр в DAG и S2S leave

- **Автодогон дыр.** Перед применением входящей транзакции проверяются `prev_events`: если предок
  неизвестен, событие **не пишется сиротой** — сервер запрашивает
  `POST /_matrix/federation/v1/get_missing_events/{roomId}` у отправителя, проверяет подпись и auth rules
  каждого полученного события и применяет их **по порядку depth**, затем исходное. Ограничения:
  ≤3 раундов, ≤50 догнанных событий; не удалось закрыть — `500` (отправитель повторит позже),
  в логе видно `origin`, `room_id`, сколько запросили/получили.
- **S2S leave в обе стороны.**
  - входящая: `GET /_matrix/federation/v1/make_leave/{roomId}/{userId}` (шаблон `m.room.member(leave)`)
    и `PUT /send_leave/{roomId}/{eventId}` — проверка подписи/auth, запись membership `leave` и `room_state`;
  - исходящая: `POST /_matrix/client/v3/rooms/{roomId}/leave` для комнаты с чужим доменом идёт через
    `make_leave → подпись → send_leave` и локально применяет membership `leave` (+ `/sync` это видит).
- Тесты: догон (предок появляется в БД раньше потомка), «сирота» не пишется (500), входящий leave,
  исходящий leave против httptest-сервера (`send_leave` вызван, membership стал `leave`).

## CI

GitHub Actions. `.github/workflows/ci.yml` запускается на push в `main` и на pull request:

| Джоба | Что проверяет |
|---|---|
| `build` | `make build` и сборка продового образа `docker build --target prod` |
| `lint` | `go vet` и golangci-lint v2.15.0 (конфиг `.golangci.yml`) |
| `gates` | тесты ratchet-гейтов на искусственно ухудшенных данных (`make gates-test`) |
| `test` | `go test -race -shuffle=on` с покрытием + coverage-ratchet: общий процент и пакеты не ниже `test/coverage-baseline.txt` |
| `integration` | HTTP API и хранилище с импортом в отдельных базах PostgreSQL 17, число SQL-запросов `/sync` и восстановление резервной копии с медиа |
| `alerts` | `promtool check config` и `promtool test rules` для `deploy/monitoring` |
| `shellcheck` | скрипты эксплуатации (бэкап, восстановление, smoke, приемка миграции) |
| `vuln` | govulncheck (не блокирует) |

`.github/workflows/complement.yml` - Complement и ratchet (`scripts/complement.sh`), ручной запуск
(workflow_dispatch); артефакты - `complement-results.jsonl` и лог `go test`. Perf-ratchet `/sync` в CI
не запускается (baseline привязан к железу) - локально `make perf`.

## Участие в разработке

Изменения - через pull request в `main`. Перед PR - `make check`, при доступной PostgreSQL -
`make test-integration SYNGOD_TEST_DSN=...` (имя базы должно содержать `test`).

## Лицензия и доступность исходного кода

syngo — свободное ПО, распространяемое по
[GNU Affero General Public License версии 3 или (по вашему выбору) любой более поздней версии](LICENSE)
(`AGPL-3.0-or-later`). Производные от Synapse части и уведомления об авторских правах
перечислены в [NOTICE](NOTICE).

Соответствующий исходный код опубликован на <https://github.com/aisa-it/syngo>.
`GET /_syngo/v1/source` публично возвращает SPDX-идентификатор лицензии и ссылку на исходники
даже при выключенной федерации; version endpoint федерации возвращает те же поля. В релизных
сборках нужно задать `COMMIT` и опубликовать эту ревизию, чтобы ссылка указывала на точные
исходники. Оператор изменённой сетевой версии обязан обеспечить её пользователям доступ
к соответствующему исходному коду по разделу 13 AGPL.
