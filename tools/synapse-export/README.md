# Экспорт из Synapse в JSONL (ADR-0006)

Полного экспортного API у Synapse нет, поэтому дамп собирается с read-only реплики БД.

```bash
export SYNAPSE_DSN='postgresql://synapse_ro@replica/synapse'

# пользователи, комнаты, алиасы, членство, медиа (без событий)
psql "$SYNAPSE_DSN" -v ON_ERROR_STOP=1 -At -o synapse-dump.jsonl -f tools/synapse-export/export.sql

# события: все stream_ordering одним снимком, исходный PDU, манифест в конце
syngo-synapse-export -synapse-dsn "$SYNAPSE_DSN" -out events.jsonl

# медиа: каталог media_store из homeserver.yaml (repository.media_store_path)
rsync -a "$MEDIA_STORE/local_content/" ./media/local_content/
```

Формат строки - `{"kind":"user|room|room_alias|event|membership|media|manifest", ...}`; описание полей -
ADR-0006 §3. Строка `kind=event` от `syngo-synapse-export` несет метаданные из `events`
(`stream_ordering`, `depth`, `outlier`, `rejected_reason`, `soft_failed`) и исходный PDU строкой в
поле `json` (`event_json.json` как есть): из него импорт берет `content`, `prev_events`,
`auth_events` (в комнатах v1/v2 - пары `[id, {sha256}]`). После событий идут строки `kind=state` -
текущее состояние комнат из `current_state_events` (строки комнаты подряд). Последняя строка
`kind=manifest` - число событий и строк состояния, границы `stream_ordering`; без нее
`syngo-import-events` дамп не примет.

Данные пользователей (ADR-0006 §4): `device`, `access_token` (открытый токен Synapse,
импорт хранит SHA-256), `device_keys`, `one_time_key`, `fallback_key`, `cross_signing_key`,
`cross_signing_signature`, `room_keys_version`, `room_key`, `account_data` (глобальная `room_id=""`,
комнатная, теги как `m.tag`), `push_rule`, `pusher`, `filter`, `to_device`, `receipt` (`m.read` и
`m.read.private` локальных пользователей из `receipts_linearized`, `thread_id` пустой - без треда,
`ts` из `data`); `user_stats` - счетчики
по пользователю для сверки (`syngo-import -verify`). Дамп содержит открытые токены доступа -
хранить как секрет и удалить после импорта.

В строках `to_device` сохраняется `device_inbox.stream_id` источника. Две записи с одинаковым
сообщением и разными `stream_id` переносятся отдельно. Повтор того же снимка сохраняет очередь и
после подтверждения доставки в `/sync`. Старый JSONL без `stream_id` различает одинаковые
сообщения по их числу в снимке. Если прежний импортер уже частично перенёс такой дамп, повторите
его новым импортером до перехода на новый экспорт. Между разными старыми снимками без ID нельзя
отличить уже доставленную запись от новой с тем же содержимым.

Импорт в SYNGO: `syngo-import` (users/rooms/aliases и данные пользователей, затем membership),
`syngo-import-events` (события), сверка с источником - `syngo-verify-migration -synapse-dsn
"$SYNAPSE_DSN" -dsn "$PSQL_DSN"` (события) и `syngo-import -verify-only` (данные пользователей).

Фикстура живого Synapse для тестов миграции пользователей - `fixture/gen.sh` (поднимает Synapse
в Docker, наполняет через API, снимает pg_dump в `cmd/syngo-import/testdata`). `SCENARIO=v12
fixture/gen.sh` - комнаты версии 12 (room_id без домена, дополнительный создатель, замена комнаты
v11 на v12) в `synapse-1.165-v12.sql` для `TestImportSynapseRoomsV12`.

Строка `kind=media` несет `quarantined_by` (карантин переносится и соблюдается),
`url_cache` (превью ссылок - `syngo-import-media` их пропускает со статусом `url_cache`) и `sha256`
(`local_media_repository.sha256`; в старых Synapse колонки нет - поле пустое, без ошибки).
`export.sql` исполняется против схемы живого Synapse 1.165 в тестах (`TestRoundTripSynapseFixture`,
`run.sh fixture`) и на репетиции (`docs/migration-rehearsal.md`).

Ограничения:
- Медиа-байты копирует `syngo-import-media` из `media_store` (`local_content/`); миниатюры
  (`local_thumbnails/`, `local_media_repository_thumbnails`) не переносятся - syngod строит их по
  запросу; `url_cache/` и `remote_content/` (`remote_media_cache`) не переносятся.
- Выгрузка событий держит одну транзакцию REPEATABLE READ: на реплике нужен
  `hot_standby_feedback = on` или достаточный `max_standby_streaming_delay`, иначе снимок
  может быть прерван конфликтом с восстановлением (выгрузка завершится ошибкой, не молча).
