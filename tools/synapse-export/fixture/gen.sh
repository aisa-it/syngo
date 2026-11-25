#!/usr/bin/env bash
# Генерация фикстуры живого Synapse для тестов миграции пользователей.
#
#   tools/synapse-export/fixture/gen.sh [out.sql]
#   SCENARIO=v12 tools/synapse-export/fixture/gen.sh [out.sql]
#
# SCENARIO=users (по умолчанию) - данные пользователей (gen_fixture.py); SCENARIO=v12 - комнаты
# версии 12 с room_id без домена (gen_fixture_v12.py).
#
# Поднимает PostgreSQL и Synapse (образ matrixdotorg/synapse:$SYNAPSE_VERSION) на уникальных
# именах, наполняет его через Client-Server и Admin API (gen_fixture.py), снимает pg_dump
# таблиц пользователя в схему synapse и убирает контейнеры. Имена и порты меняются
# переменными окружения; порт Synapse публикуется только на 127.0.0.1.
set -euo pipefail

SYNAPSE_VERSION="${SYNAPSE_VERSION:-v1.165.0}"
PREFIX="${PREFIX:-syngo-fixture-$$}"
PORT="${PORT:-38018}"
SCENARIO="${SCENARIO:-users}"
HERE="$(cd "$(dirname "$0")" && pwd)"
case "$SCENARIO" in
users) SCRIPT=gen_fixture.py DEFAULT_OUT=synapse-1.165-users.sql ;;
v12) SCRIPT=gen_fixture_v12.py DEFAULT_OUT=synapse-1.165-v12.sql ;;
*) echo "неизвестный SCENARIO: $SCENARIO" >&2; exit 2 ;;
esac
OUT="${1:-$HERE/../../../cmd/syngo-import/testdata/$DEFAULT_OUT}"
WORK="$(mktemp -d)"
SECRET="fixture-shared-secret"

cleanup() {
    docker rm -f "$PREFIX-synapse" "$PREFIX-pg" >/dev/null 2>&1 || true
    docker network rm "$PREFIX-net" >/dev/null 2>&1 || true
    rm -rf "$WORK"
}
trap cleanup EXIT

docker network create "$PREFIX-net" >/dev/null
docker run -d --name "$PREFIX-pg" --network "$PREFIX-net" \
    -e POSTGRES_USER=synapse -e POSTGRES_PASSWORD=synapse -e POSTGRES_DB=synapse \
    -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --lc-collate=C --lc-ctype=C" \
    postgres:16-alpine >/dev/null

until docker exec "$PREFIX-pg" pg_isready -U synapse -d synapse >/dev/null 2>&1; do sleep 1; done

cat >"$WORK/homeserver.yaml" <<YAML
server_name: "fixture.local"
pid_file: /data/homeserver.pid
listeners:
  - port: 8008
    tls: false
    type: http
    bind_addresses: ['0.0.0.0']
    resources: [{names: [client], compress: false}]
database:
  name: psycopg2
  args: {user: synapse, password: synapse, database: synapse, host: $PREFIX-pg, cp_min: 1, cp_max: 5}
media_store_path: /data/media_store
registration_shared_secret: "$SECRET"
macaroon_secret_key: "fixture-macaroon"
form_secret: "fixture-form"
signing_key_path: /data/fixture.local.signing.key
report_stats: false
trusted_key_servers: []
suppress_key_server_warning: true
enable_registration: false
refreshable_access_token_lifetime: 5m
password_config:
  pepper: "fixture-pepper"
rc_login:
  address: {per_second: 1000, burst_count: 1000}
  account: {per_second: 1000, burst_count: 1000}
  failed_attempts: {per_second: 1000, burst_count: 1000}
rc_message: {per_second: 1000, burst_count: 1000}
rc_registration: {per_second: 1000, burst_count: 1000}
rc_admin_redaction: {per_second: 1000, burst_count: 1000}
rc_joins:
  local: {per_second: 1000, burst_count: 1000}
YAML
cp "$HERE/gen_fixture.py" "$HERE/gen_fixture_v12.py" "$WORK/"
chmod -R a+rwX "$WORK"

docker run --rm --network "$PREFIX-net" -v "$WORK:/data" --entrypoint sh \
    "matrixdotorg/synapse:$SYNAPSE_VERSION" -c \
    'python -m synapse.app.homeserver --config-path /data/homeserver.yaml --generate-keys && chmod a+r /data/*.key' >/dev/null

docker run -d --name "$PREFIX-synapse" --network "$PREFIX-net" -v "$WORK:/data" \
    -p "127.0.0.1:$PORT:8008" -e SYNAPSE_CONFIG_PATH=/data/homeserver.yaml \
    "matrixdotorg/synapse:$SYNAPSE_VERSION" >/dev/null

until curl -fsS "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; do
    if [ "$(docker inspect -f '{{.State.Running}}' "$PREFIX-synapse")" != true ]; then
        docker logs "$PREFIX-synapse" >&2
        exit 1
    fi
    sleep 2
done

docker exec -w /data -e PYTHONDONTWRITEBYTECODE=1 "$PREFIX-synapse" python "/data/$SCRIPT" http://localhost:8008 "$SECRET"

docker stop "$PREFIX-synapse" >/dev/null

# Триггеры Synapse ссылаются на его функции, которых в дампе нет, - не переносятся.
# Таблицы пользователя (и комнаты с историей для export.sql целиком) переезжают в схему synapse: в тестах фикстура живет рядом со
# схемой SYNGO в одной базе.
docker exec "$PREFIX-pg" psql -q -U synapse -d synapse -c 'ALTER SCHEMA public RENAME TO synapse'

TABLES=(users profiles devices access_tokens refresh_tokens e2e_device_keys_json
    e2e_one_time_keys_json e2e_fallback_keys_json e2e_cross_signing_keys
    e2e_cross_signing_signatures e2e_room_keys_versions e2e_room_keys account_data
    room_account_data room_tags push_rules push_rules_enable pushers user_filters device_inbox
    rooms events event_json rejections redactions room_aliases room_memberships current_state_events
    local_media_repository receipts_linearized)
ARGS=()
for table in "${TABLES[@]}"; do ARGS+=(-t "synapse.$table"); done

{
    echo "-- Фикстура ($SCENARIO): таблицы живого Synapse $SYNAPSE_VERSION."
    echo "-- Сгенерирована tools/synapse-export/fixture/gen.sh, руками не править."
    echo "CREATE SCHEMA IF NOT EXISTS synapse;"
    docker exec "$PREFIX-pg" pg_dump -U synapse -d synapse --no-owner --no-privileges \
        --no-comments "${ARGS[@]}" |
        grep -v -e '^SET transaction_timeout' -e '^\\restrict' -e '^\\unrestrict' \
            -e '^SELECT pg_catalog.set_config' -e '^CREATE TRIGGER '
} >"$OUT"

echo "фикстура: $OUT"
