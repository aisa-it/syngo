# Synapse как референс

Решение зафиксировано в **ADR-0002 «Synapse — постоянный эталон поведения»** (`docs/adr/0002-synapse-is-reference.md`):
контракт объясняет спецификация, фактическое поведение показывает Synapse; при расхождении по умолчанию
воспроизводим Synapse и фиксируем отклонение в ADR. Этот файл — практика: где держать клон и как сверяться.

Мы **портируем** Synapse, а не проектируем «свой Matrix с нуля». Synapse остаётся поведенческим
референсом на всех этапах: спека задаёт контракт, Synapse — фактическое поведение, включая
краевые случаи, коды ошибок и порядок обработки. Расхождение допустимо, но должно быть
зафиксировано в `docs/adr/` с причиной.

## Где лежит референс

Локальный shallow-клон (у каждого разработчика свой путь, пин общий):

```
<work>/.refs/synapse        element-hq/synapse, pin 40f63e0 (2026-11-16), ~19 МБ
```
Sparse-checkout: `synapse/rest`, `api`, `handlers`, `storage`, `events`, `state`, `util`, `config`,
`types`, `federation`, `docs`. Этого достаточно для CS-API, схемы БД, `/sync`, state res и федерации.

Обновление референса (осознанно, под конкретное изменение - не «на latest»):

```bash
cd <work>/.refs/synapse
git fetch --depth 1 origin master && git checkout FETCH_HEAD     # новый pin
git log -1 --format='%H %ad' --date=short                        # зафиксировать в ADR
```

## Как сверяемся (проверенные приёмы)

| Что нужно | Как смотреть |
|---|---|
| маршруты CS-API | `grep -rn "client_patterns\|PATTERNS\|register_txn_path" synapse/rest/client/*.py` |
| обработка запроса | `synapse/rest/client/<группа>.py` → `on_GET`/`on_POST` → `synapse/handlers/<домен>.py` |
| схема БД | `synapse/storage/schema/main/delta/NN/*.sql` + `synapse/storage/databases/main/<домен>.py` (в `__init__` — `CREATE TABLE`/`SIMPLE_*_DESC`) |
| правила state / auth | `synapse/state/v2.py`, `synapse/events/validator.py`, `synapse/events/snapshot.py` |
| `/sync` | `synapse/handlers/sync.py`, `synapse/handlers/room.py`, `synapse/storage/databases/main/stream.py` |
| коды ошибок | `synapse/api/errors.py` (+ `synapse/api/constants.py`) |
| лимиты и rate-limit | `synapse/config/ratelimiting.py`, `synapse/api/ratelimiting.py` |
| конфиг-опции | `synapse/config/*.py` (`generate_config_section`) |

## Что берём, а что нет

- **Берём (поведение):** контракт API, коды и тела ошибок, семантику токенов/устройств, правила
  membership и power levels, порядок событий (`stream_ordering` vs topological), формат `/sync`-ответов,
  схему хранения (адаптируя под PostgreSQL + Go).
- **Не берём (реализацию):** Twisted/Deferred, replication-воркеры, Python-специфику, внутренние
  имена классов и модулей, MSC-эндпоинты, не входящие в нашу целевую спеку.
- **Перенос кода регулируется `AGPL-3.0-or-later`** (ADR-0002): сохраняем copyright и
  license-уведомления, отмечаем файл и дату модификации, фиксируем upstream pin и вносим
  производный файл в `NOTICE`. Архитектуру и внутренние классы Synapse механически не переносим.

## Обязательные артефакты сверки

1. Изменение → в описании ссылка на конкретный файл/функцию Synapse, с которой сверяемся.
2. Расхождение → ADR в `docs/adr/` (что и почему сделано иначе).
3. Для DB-схемы — таблица «наша таблица → таблица Synapse → отличие/причина» прямо в ADR.
4. Для перенесённого или адаптированного кода/тестов — copyright/AGPL-заголовок в файле,
   точная upstream-ссылка, дата модификации и запись в корневом `NOTICE`.
