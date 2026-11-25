# Эксплуатация SYNGO: бэкап, восстановление, мониторинг, алерты

Runbook для оператора прод-инсталляции (`deploy/prod/docker-compose.yml` или systemd-юнит).
Топология - один процесс syngod на базу (ADR-0009).

## 1. Что входит в бэкап

Бэкап - каталог `BACKUP_DIR/syngo-<stamp>/` (каталог 0700, файлы 0600) или один файл
`syngo-<stamp>.tar.gpg` при шифровании.

| Файл | Содержимое |
|---|---|
| `db.dump` | `pg_dump --format=custom` в экспортированном снимке |
| `media.tar.gz` | файлы медиа (временные `.upload-*` исключены) |
| `media-keys.tsv` | `storage_key` и `sha256` всех файлов медиа из того же снимка БД |
| `media-missing.txt` | в полной копии пуст. В старых копиях формата v2 мог содержать отсутствующие файлы |
| `counts.tsv` | число строк каждой таблицы `public` в снимке |
| `manifest.txt` | формат (`syngo-backup/2`), время, id снимка, версия схемы |
| `secrets/` | `BACKUP_SECRET_FILES`: `.env` (пароль БД, `SYNGOD_SYNC_TOKEN_SECRET`, секрет регистрации), `config.yaml`, ключ подписи федерации |
| `SHA256SUMS` | суммы всех файлов бэкапа |

**Единый снимок.** `backup.sh` открывает транзакцию `REPEATABLE READ`, экспортирует ее снимок
(`pg_export_snapshot`) и в нем же делает `pg_dump --snapshot`, счетчики таблиц и список файлов
медиа. Файлы медиа неизменяемы (запись во временный файл и `rename`, строка в БД - после
файла), поэтому архив, снятый после снимка, содержит все файлы, на которые ссылается дамп.
Файлы, загруженные после снимка, попадают в архив лишними (на них нет строк в дампе) и
безвредны. Если хотя бы один файл из снимка отсутствует в архиве или источник медиа не задан при
непустом списке файлов, `backup.sh` завершает запуск с ошибкой. Предыдущие копии сохраняются,
метрика `syngo_backup_last_failure_timestamp_seconds` обновляется. Проверьте указанный файл в
хранилище, восстановите его при необходимости и повторите бэкап.

**Секреты.** Без `SYNGOD_SYNC_TOKEN_SECRET` после восстановления все клиенты получают
`M_UNKNOWN_POS` и делают initial sync; без ключа подписи (федерация) сервер меняет ключ.
Поэтому `.env`, `config.yaml` и ключ входят в бэкап; бэкап хранить как секрет: шифровать
(`BACKUP_GPG_RECIPIENT`) и выносить с хоста.

## 2. Бэкап

| Сценарий | Команда |
|---|---|
| docker compose | `deploy/prod/backup-compose.sh backup` |
| без docker | `SYNGOD_DATABASE_DSN=... MEDIA_DIR=/var/syngo/media BACKUP_SECRET_FILES="/etc/syngo/env /etc/syngo/config.yaml" deploy/backup.sh` |
| расписание | `deploy/prod/syngo-backup.service` + `syngo-backup.timer` (ежесуточно 03:30), настройки - `/etc/syngo/backup.env` |

Переменные `backup.sh` (полный список - в шапке скрипта):

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `BACKUP_DIR` | `/var/backups/syngo` | каталог бэкапов (0700) |
| `KEEP` | `7` | сколько последних бэкапов хранить; старые удаляются после успешного нового |
| `BACKUP_SECRET_FILES` | compose: `.env` и `config.yaml` | файлы секретов и конфига через пробел |
| `BACKUP_GPG_RECIPIENT` | - | шифровать на открытый ключ gpg; закрытый ключ на сервере не хранить |
| `BACKUP_UPLOAD_CMD` | - | вынос копии, путь - в `"$1"`: `rsync -a -- "$1" backup@vault:/srv/syngo/` |
| `BACKUP_METRICS_FILE` | - | метрики для textfile collector node_exporter (алерты `SyngoBackup*`) |

RPO - интервал таймера (сутки). Для меньшего RPO - чаще запускать таймер или WAL-архивация
PostgreSQL (вне скоупа, раздел 8). Параллельный запуск блокируется (`flock` на
`BACKUP_DIR/.lock`), остатки прерванного запуска (`.tmp-*`) удаляются следующим.

## 3. Проверка восстановления

```bash
deploy/prod/backup-compose.sh verify /var/backups/syngo/syngo-<stamp>      # compose
SYNGOD_DATABASE_DSN=... deploy/verify-restore.sh /var/backups/syngo/syngo-<stamp>
```

`verify-restore.sh` восстанавливает бэкап через `restore.sh` во временную базу
`syngo_verify_<pid>_<rand>` (на том же сервере или на `VERIFY_DSN`) и во временный каталог
медиа (`mktemp`), затем удаляет обе. Живые база и каталог медиа не затрагиваются. Сверка - с
манифестом самого бэкапа (`counts.tsv`, `media-keys.tsv`, `SHA256SUMS`), поэтому записи после
дампа не дают ложный FAIL. Для копий формата v2 наличие каждого файла проверяется по снимку БД,
в том числе если старый скрипт записал его в `media-missing.txt`. Проверка неполной копии
завершается с ошибкой до восстановления целевой БД. Проверять каждый бэкап там, где есть закрытый ключ gpg, или не реже
раза в неделю.

## 4. Восстановление

### 4.1. docker compose (новый хост или после потери данных)

1. Скопировать бэкап на хост; для `.tar.gpg` нужен закрытый ключ (`gpg --import`).
2. Достать секреты: `gpg -d syngo-<stamp>.tar.gpg | tar -x` (или каталог бэкапа), положить
   `secrets/.env` в `deploy/prod/.env`, `secrets/config.yaml` - в `deploy/prod/config.yaml`.
   Порт и образ (`SYNGOD_PORT`, `SYNGO_IMAGE`) при необходимости поправить.
3. Пустые тома и только PostgreSQL (миграции не запускать - база должна быть пустой):
   ```bash
   docker compose -f deploy/prod/docker-compose.yml down -v   # только если тома старые и не нужны
   docker compose -f deploy/prod/docker-compose.yml create
   docker compose -f deploy/prod/docker-compose.yml up -d --wait postgres
   ```
4. Восстановить: `deploy/prod/backup-compose.sh restore /path/syngo-<stamp>[.tar.gpg]`.
   Скрипт останавливает syngod, проверяет `SHA256SUMS` до любых изменений, распаковывает и
   сверяет медиа (наличие и sha256 по снимку БД), восстанавливает базу одной транзакцией,
   сверяет счетчики таблиц и версию схемы с манифестом, затем пишет медиа в том
   (владелец 65532).
5. Запустить: `docker compose -f deploy/prod/docker-compose.yml up -d --wait` (migrate
   догонит схему, если образ новее бэкапа) и `deploy/prod/smoke.sh`.

### 4.2. Без docker

```bash
systemctl stop syngod
createdb -O syngo syngo_new        # пустая база; или RESTORE_FORCE=1 поверх старой
SYNGOD_DATABASE_DSN=postgres://.../syngo_new MEDIA_DIR=/var/syngo/media SECRETS_DIR=/etc/syngo/restored \
  deploy/restore.sh /var/backups/syngo/syngo-<stamp>
# сверить /etc/syngo/restored с /etc/syngo, поправить DSN на syngo_new
syngo-migrate -dsn "$SYNGOD_DATABASE_DSN" -up && systemctl start syngod
```

Непустые база или каталог медиа - отказ. `RESTORE_FORCE=1` - база перезаписывается
(`pg_restore --clean`), прежние файлы медиа откладываются в `MEDIA_DIR.pre-restore-<stamp>`.
Бэкапы старого формата (отдельные `db-<stamp>.dump` и `media-<stamp>.tar.gz`) - вручную:
`pg_restore --no-owner --no-privileges -d "$DSN" db-<stamp>.dump`, затем распаковать архив
медиа в пустой каталог.

## 5. Протокол проверки восстановления (20.11.2026)

Прогон `deploy/prod/backup-restore-compose.sh` (docker, отдельные compose-проекты
`syngo-brc-<pid>-a/b`, порты 48100-48103, все удалено после прогона):

| Шаг | Результат |
|---|---|
| 1. Проект A: образ из текущих исходников, `up --wait`, alice, комната, 7 сообщений, медиа 50 КБ | ok |
| 2. `backup-compose.sh backup` с gpg (временный ключ), bob пишет во время бэкапа | `syngo-<stamp>.tar.gpg`, права 600, метрика `syngo_backup_last_success_timestamp_seconds` записана |
| 3. После бэкапа: 2 сообщения и новый файл медиа; `verify` | OK: 35 таблиц совпали с `counts.tsv`, медиа - с `media-keys.tsv`; том медиа A побайтно не изменился |
| 4. Копия бэкапа с одним испорченным байтом в `media.tar.gz` | FAIL «контрольные суммы не сошлись» до распаковки и записи в базу |
| 5. Проект B с нуля: `.env` из бэкапа, `create`, `up postgres`, `restore`, `up --wait` | вход alice прежним паролем, в комнате ровно 7 сообщений (написанных после бэкапа нет), медиа совпадает побайтно, `/sync` с токеном проекта A - 200 |
| 6. Второй `syngod` на базе B (`compose run`) | отказ старта: `database is already used by another syngod instance (application_name=syngod-instance host=... pid=1 id=... client=172.28.0.3 backend_pid=183 since=...)` |

До правки (прежние скрипты, тот же сценарий на отдельном PostgreSQL): после одного
пользователя, созданного после дампа, `verify-restore.sh` - `FAIL: users: в исходной БД 73,
в восстановленной 72`; архив медиа распакован в рабочий каталог медиа; файлы бэкапа 0664,
секретов и конфига в бэкапе нет.

Дополнительно проверено на отдельном PostgreSQL (контейнер, порт 45432): бэкап под
параллельной записью 30 пользователей проходит verify (снимок согласован), ротация `KEEP=2`,
вынос `BACKUP_UPLOAD_CMD`, метрика неудачного запуска при неверном пароле БД (время успеха
сохраняется), отказ restore в непустую базу.

## 6. Мониторинг

Метрики - `GET /metrics` на служебном листенере `server.ops_addr` (в compose
`127.0.0.1:9108`), scrape-конфиг - `deploy/monitoring/prometheus.yml`, дашборд -
`deploy/monitoring/grafana-dashboard.json` (16 панелей). Квантили считаются в Prometheus по
гистограммам: `histogram_quantile(0.99, sum by (le) (rate(<метрика>_bucket[5m])))`.

| Метрика | Тип | Что показывает |
|---|---|---|
| `syngo_ready`, `syngo_ready_check{check}` | gauge | готовность как `/health/ready` и по проверкам (`database`, `schema`, `instance_lock`, `draining`) |
| `syngo_instance_lock_held` | gauge | блокировка экземпляра удерживается (ADR-0009) |
| `syngo_http_requests_total{method,route,code}` | counter | запросы публичного листенера по шаблону маршрута и коду |
| `syngo_http_request_duration_seconds{method,route}` | histogram | длительность запросов (для `/sync` - вместе с long-poll) |
| `syngo_sync_duration_seconds` | histogram | обработка `/sync` **без** ожидания long-poll (SLO ADR-0005) |
| `syngo_sync_requests_total`, `syngo_sync_longpoll_waiters` | counter, gauge | запросы `/sync` и long-poll, ждущие уведомления |
| `syngo_db_pool_connections{state}`, `syngo_db_pool_max_connections` | gauge | пул PostgreSQL: занятые, свободные, создаваемые; предел |
| `syngo_db_pool_acquires_total`, `syngo_db_pool_empty_acquires_total`, `syngo_db_pool_acquire_wait_seconds_total` | counter | выдачи соединений, выдачи с ожиданием, суммарное ожидание |
| `syngo_db_size_bytes` | gauge | размер базы |
| `syngo_media_disk_free_bytes`, `syngo_media_disk_size_bytes` | gauge | место на томе медиа |
| `syngo_push_queued_notifications`, `syngo_push_active_queues`, `syngo_push_max_active_queues` | gauge | очередь доставки push |
| `syngo_push_notifications_total{result}`, `syngo_push_retries_total`, `syngo_push_rejected_total` | counter | исход доставки push |
| `syngo_federation_outbox_depth`, `..._oldest_seconds`, `..._attempts_total` | gauge, counter | очередь федерации (при `federation.enabled`) |
| `syngo_rate_limited_total` | counter | ответы 429 |
| `syngo_state_resolution_*` | counter | state resolution |
| `syngo_build_info{version,commit,goversion}`, `process_start_time_seconds` | gauge | версия и время старта |
| `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_sys_bytes`, `go_gc_cycles_total`, `go_gc_pause_seconds_total` | gauge, counter | рантайм Go |
| `syngo_backup_last_success_timestamp_seconds`, `..._failure_...`, `syngo_backup_last_size_bytes`, `syngo_backup_last_duration_seconds` | gauge | пишет `backup.sh` в `BACKUP_METRICS_FILE` (node_exporter textfile collector) |

Место на томе PostgreSQL и каталоге бэкапов - метрики node_exporter
(`node_filesystem_avail_bytes`), алерт `SyngoHostDiskLow`.

### 6.1. Пул PostgreSQL

| Параметр | По умолчанию | Рекомендация |
|---|---|---|
| `database.max_conns` | 20 | 2-3 соединения на ядро сервера PostgreSQL; сумма по всем клиентам базы (syngod, бэкап, миграции, админ) - не больше `max_connections` минус `superuser_reserved_connections` (по умолчанию 100 и 3). Больше 4 на ядро не дает пропускной способности: запросы конкурируют за CPU и WAL |

Признаки нехватки пула: растут `rate(syngo_db_pool_empty_acquires_total[5m])` и
`rate(syngo_db_pool_acquire_wait_seconds_total[5m])`, при этом запросы в
`pg_stat_statements` быстрые - увеличить `database.max_conns` (в пределах
рекомендации выше). Если медленные сами запросы - пул не поможет, нужна оптимизация.
Нагрузочная проверка - `make load PROFILE=M` (docs/load-testing.md).

## 7. Алерты

Правила - `deploy/monitoring/alerts.yml`, тесты - `deploy/monitoring/alerts_test.yml`
(`promtool test rules`, команда в шапке файла).

| Алерт | Условие | Важность |
|---|---|---|
| `SyngoDown` | `up == 0` 2 мин | critical |
| `SyngoNotReady` | `syngo_ready == 0` 2 мин | critical |
| `SyngoInstanceLockLost` | `syngo_instance_lock_held == 0` 1 мин | critical |
| `SyngoRestarting` | больше 3 рестартов за 30 мин | warning |
| `SyngoHigh5xxRate` | 5xx больше 5% запросов 5 мин (при трафике от 0.1 rps) | critical |
| `SyngoSlowRequestsP99` | p99 API без `/sync` и медиа выше 1 с 10 мин | warning |
| `SyngoSyncSlowP99` | p99 обработки `/sync` выше 1 с 10 мин | warning |
| `SyngoMediaDiskLow`, `SyngoMediaDiskCritical` | свободно меньше 10% / 3% на томе медиа | warning / critical |
| `SyngoHostDiskLow` | свободно меньше 10% на томах хоста (node_exporter) | warning |
| `SyngoDBPoolSaturated` | пул занят больше 90% 10 мин | warning |
| `SyngoBackupMissing`, `SyngoBackupNeverRan` | нет успешного бэкапа 26 ч | critical |
| `SyngoBackupFailed` | последний запуск бэкапа с ошибкой | critical |
| `SyngoFederationOutboxStuck`, `SyngoFederationOutboxUnavailable` | очередь федерации стоит 10 мин / статистика недоступна | warning |
| `SyngoPushDeliveryFailing` | больше половины push не доставлено 15 мин | warning |
| `SyngoPushQueueBacklog` | в очереди push больше 500 или занято больше 80% очередей 10 мин | warning |
| `SyngoPushDropped` | push отброшены за 15 мин | warning |

### SyngoDown

Процесс не отвечает на `/metrics`. `docker compose ps`, `docker compose logs syngod --tail=100`
(или `journalctl -u syngod`). Частые причины старта: невалидный конфиг, схема не накатана
(`syngo-migrate -up`), каталог медиа недоступен, на базе уже работает другой syngod
(`database is already used by another syngod instance` - см. SyngoInstanceLockLost).

### SyngoNotReady

`curl -s 127.0.0.1:9108/health/ready` показывает причину. `database` - PostgreSQL недоступен
(`docker compose ps postgres`, место на диске); `schema` - версия схемы не совпадает с
бинарем (накатить миграции или откатить образ); `instance_lock` - см. ниже; `draining` -
идет остановка.

### SyngoInstanceLockLost

Соединение с блокировкой экземпляра оборвано и не восстановлено. Если в логе
`instance lock lost` - базу занял другой процесс syngod: найти его по `application_name`
(`SELECT pid, application_name, client_addr FROM pg_stat_activity WHERE application_name LIKE 'syngod-instance%'`)
и остановить лишний; два экземпляра на одной базе не поддерживаются (ADR-0009).

### SyngoHigh5xxRate

`sum by (route, code) (rate(syngo_http_requests_total{code=~"5.."}[5m]))` - какие маршруты.
Логи: `docker compose logs syngod | grep '"status":5'` (поле `route`). Часто - база
(SyngoDBPoolSaturated, SyngoNotReady) или место на томе медиа (upload).

### SyngoSyncSlowP99

Обработка `/sync` медленная без учета long-poll: нагрузка на базу (пул, медленные запросы
в `pg_stat_statements`), initial sync больших аккаунтов. SLO - ADR-0005 §2.

### SyngoMediaDiskLow

Расширить том медиа или почистить старые медиа (admin API). При 0 свободного места upload
отвечает 500, а бэкап не создается.

### SyngoBackupMissing

`systemctl status syngo-backup.service`, `journalctl -u syngo-backup.service`. Частые причины:
нет места в `BACKUP_DIR`, PostgreSQL недоступен, ошибка выноса (`BACKUP_UPLOAD_CMD`), нет
ключа gpg. Запустить вручную: `systemctl start syngo-backup.service`.

### SyngoPushQueueBacklog

Шлюз push (sygnal) отвечает медленно или недоступен. Очередь - в памяти процесса: при
рестарте недоставленное теряется, при переполнении - отбрасывается (SyngoPushDropped).
Проверить доступность шлюза из контейнера и `syngo_push_notifications_total{result="failed"}`.

## 8. Отложено

| Что | Почему |
|---|---|
| PITR (WAL-архивация, `pg_basebackup`), RPO меньше суток | нужен отдельный контур хранения WAL |
| Хранилище медиа S3 и бэкап средствами S3 (версионирование) | `media.storage_backend: s3` пока отклоняется валидацией (ADR-0009, п. 6) |
| Проверка бэкапа запуском syngod на восстановленной базе по расписанию | сейчас - сверка с манифестом; запуск и smoke - в `backup-restore-compose.sh` |
| Экспортер PostgreSQL (`postgres_exporter`) и алерты по репликации | репликации нет; размер базы - `syngo_db_size_bytes` |
| Метрики очереди push после выноса в БД | очередь в памяти до ADR-0009, п. 3 |
