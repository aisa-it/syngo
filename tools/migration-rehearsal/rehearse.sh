#!/usr/bin/env bash
# Репетиция миграции на живом Synapse.
#
#   ./tools/migration-rehearsal/rehearse.sh <syngo-test-dsn> [all|up|populate|cutover|rollback|down]
#
# all (по умолчанию): поднять Synapse той же версии, что в фикстуре
# (internal/importevents/testdata), и PostgreSQL под ним в docker на уникальных
# именах и свободных портах; наполнить данными (populate.py); остановить Synapse
# (начало окна простоя) и пройти runbook скриптом приемки
# (tools/migration-acceptance/run.sh, режим synapse) в тестовую базу SYNGO;
# затем репетиция отката - Synapse снова запускается, вход и отправка работают;
# в конце все контейнеры, сеть и каталог данных удаляются (KEEP=1 - оставить).
#
# База SYNGO - только тестовая (имя содержит test): run.sh стирает ее схему.
# Переменные: PREFIX (имена контейнеров), SYNAPSE_PORT, PG_PORT, SYNGO_PORT,
# SYNAPSE_IMAGE, SCALE (множитель числа сообщений), WORK (каталог данных),
# REPORT (файл итогов приемки).
set -euo pipefail

cd "$(dirname "$0")/../.."

DSN=${1:?нужен DSN тестовой базы SYNGO}
PHASE=${2:-all}

PREFIX=${PREFIX:-syngo-rh}
SYNAPSE_IMAGE=${SYNAPSE_IMAGE:-matrixdotorg/synapse:v1.165.0}
PG_IMAGE=${PG_IMAGE:-postgres:16-alpine}
SYNAPSE_PORT=${SYNAPSE_PORT:-38608}
PG_PORT=${PG_PORT:-35632}
SYNGO_PORT=${SYNGO_PORT:-38680}
SERVER_NAME=${SERVER_NAME:-rh.test}
SCALE=${SCALE:-1}
WORK=${WORK:-/tmp/$PREFIX}
NET=$PREFIX-net
PG=$PREFIX-pg
SYN=$PREFIX-synapse
WEB=$PREFIX-web
SYNAPSE_URL=http://127.0.0.1:$SYNAPSE_PORT
SYNAPSE_DSN="postgres://synapse:synapse@127.0.0.1:$PG_PORT/synapse?sslmode=disable"

say() { echo "[$(date +%H:%M:%S)] $*"; }
now() { date +%s.%N; }
elapsed() { python3 -c "print(round($2-$1,2))"; }

wait_http() {
  for _ in $(seq 1 120); do
    curl -sf -o /dev/null "$1" && return 0
    sleep 1
  done
  echo "не дождались $1" >&2
  return 1
}

up() {
  for p in "$SYNAPSE_PORT" "$PG_PORT" "$SYNGO_PORT"; do
    if ss -ltn | awk '{print $4}' | grep -qE "[:.]$p$"; then echo "порт $p занят" >&2; exit 2; fi
  done
  mkdir -p "$WORK/data" "$WORK/web"
  docker network create "$NET" >/dev/null
  docker run -d --name "$PG" --network "$NET" -p "127.0.0.1:$PG_PORT:5432" \
    -e POSTGRES_USER=synapse -e POSTGRES_PASSWORD=synapse -e POSTGRES_DB=synapse \
    -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --locale=C" "$PG_IMAGE" >/dev/null

  # Страница для превью ссылки: og:image скачивается Synapse в url_cache.
  python3 - "$WORK/web" <<'PY'
import struct, sys, zlib
root = sys.argv[1]
def chunk(k, p): return struct.pack(">I", len(p)) + k + p + struct.pack(">I", zlib.crc32(k + p) & 0xFFFFFFFF)
raw = b"".join(b"\x00" + b"".join(bytes([(x * 7) % 256, (y * 3) % 256, 200]) for x in range(64)) for y in range(64))
png = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 64, 64, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")
open(root + "/og.png", "wb").write(png)
open(root + "/page.html", "w").write('<html><head><title>Репетиция</title><meta property="og:title" content="Репетиция миграции"><meta property="og:image" content="og.png"></head><body>превью</body></html>')
PY
  docker run -d --name "$WEB" --network "$NET" -v "$WORK/web:/web:ro" -w /web \
    --entrypoint python "$SYNAPSE_IMAGE" -m http.server 8000 >/dev/null

  docker run --rm -v "$WORK/data:/data" -e UID="$(id -u)" -e GID="$(id -g)" \
    -e SYNAPSE_SERVER_NAME="$SERVER_NAME" -e SYNAPSE_REPORT_STATS=no "$SYNAPSE_IMAGE" generate >/dev/null
  python3 - "$WORK/data/homeserver.yaml" "$PG" <<'PY'
import re, sys
path, pg = sys.argv[1], sys.argv[2]
text = open(path).read()
text = re.sub(r"database:\n(  .*\n)+", "database:\n  name: psycopg2\n  args:\n    user: synapse\n    password: synapse\n"
              f"    database: synapse\n    host: {pg}\n    cp_min: 2\n    cp_max: 10\n", text)
text += """
trusted_key_servers: []
suppress_key_server_warning: true
enable_authenticated_media: true
max_upload_size: 50M
url_preview_enabled: true
url_preview_ip_range_blacklist: ['255.255.255.255/32']
rc_message: {per_second: 10000, burst_count: 100000}
rc_registration: {per_second: 10000, burst_count: 100000}
rc_login:
  address: {per_second: 10000, burst_count: 100000}
  account: {per_second: 10000, burst_count: 100000}
  failed_attempts: {per_second: 10000, burst_count: 100000}
rc_joins:
  local: {per_second: 10000, burst_count: 100000}
rc_invites: {per_room: {per_second: 10000, burst_count: 100000}, per_user: {per_second: 10000, burst_count: 100000}}
rc_admin_redaction: {per_second: 10000, burst_count: 100000}
rc_media_create: {per_second: 10000, burst_count: 100000}
"""
open(path, "w").write(text)
PY
  # PostgreSQL должен принять подключения раньше Synapse.
  for _ in $(seq 1 60); do docker exec "$PG" pg_isready -U synapse -q && break; sleep 1; done
  docker run -d --name "$SYN" --network "$NET" -p "127.0.0.1:$SYNAPSE_PORT:8008" \
    -e UID="$(id -u)" -e GID="$(id -g)" -v "$WORK/data:/data" "$SYNAPSE_IMAGE" >/dev/null
  wait_http "$SYNAPSE_URL/health"
  say "Synapse $(curl -s "$SYNAPSE_URL/_matrix/federation/v1/version" | python3 -c 'import json,sys;print(json.load(sys.stdin)["server"]["version"])' 2>/dev/null || echo '?') на $SYNAPSE_URL, PostgreSQL на 127.0.0.1:$PG_PORT"
}

populate() {
  docker exec "$SYN" register_new_matrix_user -c /data/homeserver.yaml -u alice -p pass-alice -a http://localhost:8008 >/dev/null
  local t0 t1
  t0=$(now)
  python3 tools/migration-rehearsal/populate.py "$SYNAPSE_URL" "$SERVER_NAME" "http://$WEB:8000/page.html" "$SCALE" >"$WORK/marks.json"
  t1=$(now)
  say "наполнение: $(elapsed "$t0" "$t1") с"
  docker exec "$PG" psql -U synapse -d synapse -At -F ' ' -c "
    select 'users', count(*) from users union all
    select 'rooms', count(*) from rooms union all
    select 'events', count(*) from events union all
    select 'redactions', count(*) from redactions union all
    select 'local_media', count(*) from local_media_repository union all
    select 'url_cache', count(*) from local_media_repository where url_cache is not null union all
    select 'quarantined', count(*) from local_media_repository where quarantined_by is not null union all
    select 'thumbnails', count(*) from local_media_repository_thumbnails" | tee "$WORK/volumes.txt"
  du -sh "$WORK/data/media_store" | tee -a "$WORK/volumes.txt"
}

cutover() {
  # Окно простоя: Synapse остановлен (не read-only на прокси: блокировка POST
  # ломает вход, а остановленный Synapse гарантирует согласованный снимок).
  local t0 t1
  # Команды собираются до окна: в простой входит только сам переезд.
  mkdir -p "$WORK/bin"
  go build -o "$WORK/bin/" ./cmd/migrate ./cmd/syngod ./cmd/syngo-import ./cmd/syngo-import-events \
    ./cmd/syngo-import-media ./cmd/syngo-synapse-export ./cmd/syngo-verify-migration ./cmd/synapse-fixture
  t0=$(now)
  docker stop "$SYN" >/dev/null
  say "Synapse остановлен - начало окна простоя"
  local quarantined
  quarantined=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["quarantined"][0])' "$WORK/marks.json")
  local alias="#general:$SERVER_NAME"
  SYNAPSE_DSN="$SYNAPSE_DSN" MEDIA_STORE="$WORK/data/media_store" SERVER_NAME="$SERVER_NAME" \
    ACCEPT_USER=alice ACCEPT_PASSWORD=pass-alice ACCEPT_ROOM_ALIAS="$alias" \
    ACCEPT_QUARANTINED="$quarantined" PORT="$SYNGO_PORT" REPORT="${REPORT:-$WORK/acceptance.md}" KEEP="${KEEP:-}" \
    BIN="$WORK/bin" \
    ./tools/migration-acceptance/run.sh "$DSN" synapse
  t1=$(now)
  say "окно простоя (остановка Synapse - приемка пройдена): $(elapsed "$t0" "$t1") с"
}

rollback() {
  # Откат до включения записи в SYNGO: Synapse не менялся, его просто запускают.
  local t0 t1
  t0=$(now)
  docker start "$SYN" >/dev/null
  wait_http "$SYNAPSE_URL/health"
  local token room event
  token=$(curl -sf -X POST -H 'Content-Type: application/json' \
    -d '{"type":"m.login.password","identifier":{"type":"m.id.user","user":"alice"},"password":"pass-alice"}' \
    "$SYNAPSE_URL/_matrix/client/v3/login" | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
  room=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["rooms"]["general"])' "$WORK/marks.json")
  event=$(curl -sf -X PUT -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d '{"msgtype":"m.text","body":"после отката"}' \
    "$SYNAPSE_URL/_matrix/client/v3/rooms/$room/send/m.room.message/rollback1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["event_id"])')
  curl -sf -H "Authorization: Bearer $token" "$SYNAPSE_URL/_matrix/client/v3/rooms/$room/messages?dir=b&limit=1" |
    python3 -c 'import json,sys;c=json.load(sys.stdin)["chunk"];sys.exit(0 if c and c[0]["event_id"]==sys.argv[1] else 1)' "$event"
  t1=$(now)
  say "откат: Synapse снова принимает вход и запись, $(elapsed "$t0" "$t1") с"
}

down() {
  docker rm -f "$SYN" "$WEB" "$PG" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  say "стенд репетиции удален"
}

case "$PHASE" in
  all)
    [ -z "${KEEP:-}" ] && trap down EXIT
    up; populate; cutover; rollback ;;
  up) up ;;
  populate) populate ;;
  cutover) cutover ;;
  rollback) rollback ;;
  down) down ;;
  *) echo "неизвестная фаза: $PHASE" >&2; exit 2 ;;
esac
