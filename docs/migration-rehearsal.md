# Протокол репетиции миграции на живом Synapse

Репетиция cutover по `docs/migration-runbook.md` на живом Synapse той же версии, что в фикстуре
(`internal/importevents/testdata`), с репетицией отката. Повторить:

```bash
SCALE=3 REPORT=acceptance.md ./tools/migration-rehearsal/rehearse.sh \
  'postgres://syngo:syngo@127.0.0.1:5432/syngo_test?sslmode=disable' all
```

Скрипт поднимает в docker PostgreSQL 16 и `matrixdotorg/synapse:v1.165.0` (контейнеры `syngo-rh-*`,
порты 38608/35632/38680 - меняются переменными), наполняет Synapse через Client-Server и admin API
(`populate.py`), останавливает Synapse и проходит runbook скриптом приемки
(`tools/migration-acceptance/run.sh`, профиль `synapse`), затем запускает Synapse обратно (откат) и
удаляет контейнеры, сеть и каталог данных. База SYNGO - только тестовая.

## Прогон 20.11.2026

| Параметр | Значение |
|---|---|
| Synapse | 1.165.0 (`matrixdotorg/synapse:v1.165.0`), PostgreSQL 16, локальное хранилище медиа |
| SYNGO | версия от 20.11.2026, PostgreSQL 17 (тестовая база), syngod на 127.0.0.1:38680 |
| Машина | 8 ядер, общая с другими сборками (load average 3-10) |
| Наполнение | 401.6 с через API (`SCALE=3`) |

### Объем источника

| Данные | Synapse |
|---|---|
| Пользователи | 20 (1 администратор, 1 деактивирован) |
| Комнаты | 4: v10 публичная с алиасом, v10 приватная, v11, v1 (ссылки prev/auth парами) |
| События | 1094 (сообщения, ответы, правки, реакции, смена темы, приглашения, вход и выход) |
| Redaction | 30 (своих сообщений и модератором) |
| Медиа | 27 строк `local_media_repository`: 20 PNG, 4 бинарных, 1 файл 3 МиБ; 2 в карантине (admin API), 2 - превью ссылки (`url_cache`); 67 миниатюр; `media_store` 8.2 МиБ |
| Алиасы, членство | 2 алиаса, 37 строк текущего членства |

### Окно простоя (Synapse остановлен)

| Шаг runbook | Время, с | Результат |
|---|---|---|
| 3.1 схема (`migrate -up` на пустую базу) | - | версия 35 |
| 3.2 `export.sql` | 0.25 | 90 строк: пользователи, комнаты, алиасы, членство, медиа |
| 3.2 `syngo-synapse-export` | 0.21 | 1160 строк: 1094 события, 65 строк состояния, манифест |
| 3.3 пользователи, комнаты, алиасы | 0.55 | ошибок 0 |
| 3.4 события | 1.82 | вставлено 1094, bad 0, event_id mismatch 0, redacted 30 |
| 3.5 членство | 0.54 | ошибок 0 |
| 3.6 медиа | 0.23 | copied=25, url_cache=2, quarantined=2, missing/mismatch/hash_mismatch/failed=0, 5 139 563 байт |
| 3.6 медиа повторно | 0.26 | copied=0, skipped=25 |
| 3.7 сверка `syngo-verify-migration` | - | `result: OK` (ниже) |
| 4 syngod и проверки через API | - | все прошли (ниже) |
| **Итого: от остановки Synapse до пройденной приемки** | **18.5** | команды собраны до окна |

Сверка с БД Synapse:

```
events: synapse=1094 syngo=1094
stream_ordering: synapse=[2..1095] syngo=[2..1095] sequence=1095
rooms: checked=4 count_diffs=0
event_id: missing=0 extra=0 class_diffs=0
state: rows=65 diffs=0
redactions: redacted=30 diffs=0
memberships: rows=37 diffs=0
users: rows=20 diffs=0; aliases: rows=2 diffs=0
media: rows=25 diffs=0 url_cache_skipped=2
result: OK
```

Проверки через API syngod:

| Проверка | Результат |
|---|---|
| `/health` | 200 |
| вход `alice` паролем из Synapse (bcrypt) | токен получен |
| `/messages` общей комнаты, 5 последних | те же `event_id` и порядок, что в БД |
| `/sync` | 145 событий состояния и timeline |
| скачивание медиа (файл 3 МиБ) через `/_matrix/client/v1/media/download` | sha256 совпал с записанным при импорте и с Synapse |
| медиа в карантине | 404 |
| `events_stream_ordering_seq` ≥ max(stream_ordering) | да |

### Откат (runbook 6.1)

Synapse запущен снова: вход `alice`, отправка сообщения и чтение его через `/messages` - **9.9 с** от
запуска контейнера. Synapse за время окна не изменился (остановлен), БД SYNGO выбрасывается.

### Отрицательные проверки

- `run.sh synapse` с неверным `ACCEPT_PASSWORD`: `FAILED login alice`, `ПРИЕМКА НЕ ПРОЙДЕНА`, код 1
  (раньше скрипт печатал FAILED и выходил с 0).
- Расхождения медиа (`TestImportMediaDiscrepancies`): размер и sha256 - код 1, строки `problem:`.

## Выводы

1. Runbook исполним шаг за шагом на живом Synapse: выгрузка, импорт, медиа, сверка и проверки
   через API проходят без ручных правок; откат до включения записи - запуск Synapse.
2. Простой на этом объеме - секунды, он определяется копированием медиа и импортом событий. Для
   боевого объема: события - по замерам синтетики (~35 тыс./с, `docs/migration-runbook.md`), медиа -
   перенести заранее (повторный запуск в окне копирует только новое) и прогнать `run.sh synapse` на
   копии боевой БД до назначения окна.
3. Карантин сохранен (404), `url_cache` исключен без ошибки, sha256 сверены с Synapse, повторный
   запуск медиа ничего не копирует.

## Что не проверено репетицией

- Объем боевого Synapse и S3-хранилище медиа: репетиция на копии боевой БД - перед окном.
- Устройства, токены, ключи E2EE, `account_data` - их перенос в репетицию не входил (импорт данных пользователей, `docs/migration-runbook.md`).
- Федерация - вне скоупа (`remote_media_cache` не переносится).
