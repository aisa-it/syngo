# Фикстура живого Synapse

`synapse-1.165.sql` - таблицы живого Synapse 1.165.0 на PostgreSQL 16 (`events`, `event_json`,
`rejections`, `redactions`, `current_state_events`, `rooms`, `room_memberships`, `users`,
`room_aliases`, `profiles`) в схеме `synapse_fixture`. Интеграционные тесты пакета грузят ее в
тестовую базу и гоняют на ней `syngo-synapse-export` (пакет `internal/synapseexport`), импорт и сверку.

Что внутри:

| Комната | Версия | Сценарий |
|---|---|---|
| Общая (`#general`) | 10 | 3 участника, сообщения, смена темы, `\u0000` в content, redaction своего и чужого (админом) сообщения, ответ, выход участника |
| Старая v1 | 1 | ссылки prev/auth парами `[id, {sha256}]`, event_id не хеш, redaction |
| Новая v11 | 11 | `redacts` в content, redaction |

Правки `synthetic.sql` (без федерации их не получить): отклоненное событие (`rejections` и
`events.rejection_reason`), soft-failed (`event_json.internal_metadata`), outlier, backfill
(`stream_ordering = -1`), форк темы, разрешенный в пользу старого события (`current_state_events`
указывает не на последнее по stream_ordering).

`synapse-1.165-media.sql` - `local_media_repository` того же Synapse 1.165 (снят на репетиции
миграции, `tools/migration-rehearsal`) в схеме `synapse_fixture`, грузится после основной
фикстуры: два PNG, бинарный файл, медиа в карантине и две строки превью ссылки (`url_cache`);
`user_id` переведены на пользователей фикстуры. Файлы - `synapse-media/local_content` (раскладка
`media_store`, файлов превью нет - они не переносятся). Используют `TestRoundTripSynapseFixture`
(`cmd/syngo-import`) и `tools/migration-acceptance/run.sh fixture`.

`synapse-1.165-userdata-schema.sql` - пустые таблицы данных пользователей того же Synapse 1.165
(устройства, токены, E2EE, бэкап ключей, account data, push rules, pushers, фильтры, to-device;
DDL из `cmd/syngo-import/testdata/synapse-1.165-users.sql`) в схеме `synapse_fixture`:
с ними `export.sql` исполняется на этой фикстуре целиком. `receipts_linearized` в pg_dump
не было - DDL по схеме Synapse, строки отметок добавляет `TestImportSynapseReceipts`. Данные пользователей проверяет
`TestImportSynapseUsers` на своей фикстуре (`tools/synapse-export/fixture/gen.sh`).

## Как пересобрать

```bash
docker network create fx-net
docker run -d --name fx-pg --network fx-net -e POSTGRES_USER=synapse -e POSTGRES_PASSWORD=synapse \
  -e POSTGRES_DB=synapse -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --locale=C" postgres:16-alpine
docker run --rm -v "$PWD/fx-data:/data" -e SYNAPSE_SERVER_NAME=c4.test -e SYNAPSE_REPORT_STATS=no \
  matrixdotorg/synapse:v1.165.0 generate
# homeserver.yaml: database psycopg2 (host fx-pg), trusted_key_servers: [], rc_* с большими лимитами
docker run -d --name fx-synapse --network fx-net -p 127.0.0.1:38008:8008 -v "$PWD/fx-data:/data" \
  matrixdotorg/synapse:v1.165.0
for u in alice bob carol; do
  docker exec fx-synapse register_new_matrix_user -c /data/homeserver.yaml -u $u -p pass-$u \
    $([ $u = alice ] && echo -a || echo --no-admin) http://localhost:8008
done
python3 make-fixture.py http://127.0.0.1:38008 c4.test > marks.json
# synthetic.sql: подставить event_id из marks.json, затем
docker exec -i fx-pg psql -U synapse -d synapse -v ON_ERROR_STOP=1 < synthetic.sql
docker exec fx-pg pg_dump -U synapse -d synapse --no-owner --no-privileges --column-inserts \
  --rows-per-insert=1 -t events -t event_json -t rejections -t redactions -t current_state_events \
  -t rooms -t room_memberships -t users -t room_aliases -t profiles > raw.sql
# в synapse-1.165.sql: шапка с DROP/CREATE SCHEMA synapse_fixture, без строк \restrict, SET,
# set_config и CREATE TRIGGER, public. -> synapse_fixture.
```
