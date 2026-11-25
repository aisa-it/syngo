#!/usr/bin/env bash
# Приёмка миграции: импорт по runbook и проверки, код возврата
# ненулевой при любой неудаче (раньше печатал FAILED и выходил с 0).
#
#   ./tools/migration-acceptance/run.sh <dsn> [профиль]
#
# Профили:
#   small | full - синтетический профиль (cmd/synapse-fixture): small для CI,
#                 full - 100 users / 200 rooms / 100k events / 500 media;
#   fixture     - фикстура живого Synapse 1.165 (internal/importevents/testdata):
#                 грузится в схему synapse_fixture той же тестовой базы и проходит
#                 весь runbook - export.sql, syngo-synapse-export, импорт, медиа
#                 (дважды: второй прогон ничего не копирует), сверка, API;
#   synapse     - то же на копии БД Synapse: SYNAPSE_DSN (копия/реплика, только
#                 чтение), MEDIA_STORE (media_store Synapse), SERVER_NAME,
#                 ACCEPT_USER/ACCEPT_PASSWORD (пользователь копии для входа),
#                 ACCEPT_ROOM_ALIAS (комната для /messages; по умолчанию - первая
#                 комната пользователя), ACCEPT_QUARANTINED (media_id в карантине,
#                 необязательно).
#
# Скрипт начинает с отката всех миграций (удаление таблиц), поэтому принимает только
# отдельную тестовую базу: имя БД в DSN должно содержать "test".
#
# Требования: PostgreSQL (psql), Go, свободный порт PORT (по умолчанию 8008).
# Отчёт: stdout + (если задан REPORT) файл с итоговыми числами; KEEP=1 - оставить
# рабочий каталог; BIN - каталог собранных заранее команд (не собирать в окне простоя).
set -euo pipefail

cd "$(dirname "$0")/../.."   # все go run ниже — из корня репозитория

DSN=${1:?нужен DSN}
PROFILE=${2:-small}
PORT=${PORT:-8008}
BASE=http://127.0.0.1:$PORT

DB_NAME=${DSN%%\?*}
DB_NAME=${DB_NAME##*/}
if [[ ${DB_NAME,,} != *test* ]]; then
  echo "отказ: база \"$DB_NAME\" не тестовая (имя должно содержать test) - скрипт стирает схему" >&2
  exit 2
fi
WORK=$(mktemp -d)
REPORT=${REPORT:-}
LOG="$WORK/acceptance.log"
BIN=${BIN:-$WORK/bin}
FAILED=0
CHECKS=()
TIMES=()

say() { echo "$@" | tee -a "$LOG"; }
step() { say ""; say "== $*"; }

# check <имя> <значение> <0|1>: проверка приемки; неудача не
# прерывает остальные проверки, но код возврата будет 1.
check() {
  local name=$1 value=$2 ok=$3
  if [ "$ok" = 1 ]; then
    say "ok      $name: $value"
    CHECKS+=("ok $name: $value")
  else
    say "FAILED  $name: $value"
    CHECKS+=("FAILED $name: $value")
    FAILED=1
  fi
}

# timed <метка> <команда...>: замер шага; ошибка команды останавливает приемку.
timed() {
  local label=$1 t0 t1 secs
  shift
  t0=$(date +%s%N)
  if ! "$@" >>"$LOG" 2>&1; then
    say "ШАГ НЕ ПРОШЕЛ: $label (код $?), хвост лога:"
    tail -15 "$LOG"
    exit 1
  fi
  t1=$(date +%s%N)
  secs=$(python3 -c "print(round(($t1-$t0)/1e9,2))")
  TIMES+=("$label=${secs}s")
  say "$label: ${secs} s"
}

cleanup() {
  [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null
  if [ "$PROFILE" = fixture ]; then
    psql "$DSN" -qc 'DROP SCHEMA IF EXISTS synapse_fixture CASCADE' >/dev/null 2>&1 || true
  fi
  if [ -n "${KEEP:-}" ]; then echo "рабочий каталог сохранён: $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

sql() { psql "$DSN" -v ON_ERROR_STOP=1 -tAc "$1"; }

build() {
  [ -x "$BIN/syngod" ] && [ -x "$BIN/syngo-verify-migration" ] && return 0
  mkdir -p "$BIN"
  go build -o "$BIN/" ./cmd/migrate ./cmd/syngod ./cmd/syngo-import ./cmd/syngo-import-events \
    ./cmd/syngo-import-media ./cmd/syngo-synapse-export ./cmd/syngo-verify-migration ./cmd/synapse-fixture
}

reset_schema() {
  step "миграции"
  # Откат всех миграций явным числом шагов: -down без N запрещен.
  local cur
  cur=$("$BIN/migrate" -dsn "$DSN" -version 2>>"$LOG" | sed -n 's/^версия: \([0-9]*\),.*/\1/p' || true)
  if [[ -n $cur && $cur -gt 0 ]]; then
    # Шагов отката - число примененных миграций, а не номер версии: в нумерации
    # бывают пропуски (номера раздаются ветками заранее).
    local steps
    steps=$(find migrations -name '*.up.sql' | sed 's#.*/0*\([0-9][0-9]*\)_.*#\1#' | awk -v c="$cur" '$1 <= c' | wc -l)
    "$BIN/migrate" -dsn "$DSN" -down "$steps" >>"$LOG" 2>&1
  fi
  "$BIN/migrate" -dsn "$DSN" -up >>"$LOG" 2>&1
}

start_server() {
  step "запуск сервера"
  # Конфиг-пример - prod по умолчанию: приемка идет в dev с server_name дампа.
  SYNGOD_ENV=dev SYNGOD_MATRIX_SERVER_NAME="$SERVER_NAME" SYNGOD_SERVER_ADDR="127.0.0.1:$PORT" \
  SYNGOD_DATABASE_DSN="$DSN" SYNGOD_SYNC_TOKEN_SECRET=acceptance-secret \
    SYNGOD_MEDIA_DIRECTORY="$MEDIA_OUT" SYNGOD_LOG_LEVEL=error SYNGOD_LOG_FORMAT=text \
    "$BIN/syngod" -config config.example.yaml >"$WORK/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 60); do
    curl -sf -o /dev/null "$BASE/health" && break
    sleep 0.5
  done
  local code
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/health" || true)
  check health "$code" "$([ "$code" = 200 ] && echo 1 || echo 0)"
  [ "$code" = 200 ] || { tail -20 "$WORK/server.log"; exit 1; }
}

login() {
  curl -s -X POST -H 'Content-Type: application/json' \
    -d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"$1\"},\"password\":\"$2\"}" \
    "$BASE/_matrix/client/v3/login" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("access_token",""))' 2>/dev/null || true
}

# api_checks <user> <password> <room_id>: вход, /messages против БД, /sync, медиа, sequence.
api_checks() {
  local user=$1 password=$2 room=$3 token
  step "проверки через API"
  token=$(login "$user" "$password")
  check "login $user" "$([ -n "$token" ] && echo ok || echo нет токена)" "$([ -n "$token" ] && echo 1 || echo 0)"
  [ -n "$token" ] || return 0

  # /messages отдает последние события комнаты в порядке stream_ordering - те же, что в БД.
  local got want
  got=$(curl -s -H "Authorization: Bearer $token" "$BASE/_matrix/client/v3/rooms/$room/messages?dir=b&limit=5" |
    python3 -c 'import json,sys;print(",".join(e["event_id"] for e in json.load(sys.stdin).get("chunk",[])))' 2>/dev/null || true)
  want=$(sql "select string_agg(event_id, ',' order by stream_ordering desc) from (select event_id, stream_ordering from events where room_id='$room' order by stream_ordering desc limit 5) t")
  check "messages $room" "${got:-пусто}" "$([ -n "$got" ] && [ "$got" = "$want" ] && echo 1 || echo 0)"

  local sync_state
  sync_state=$(curl -s -H "Authorization: Bearer $token" "$BASE/_matrix/client/v3/sync?timeout=0" | python3 -c '
import json,sys
d=json.load(sys.stdin)
rooms=(d.get("rooms") or {}).get("join") or {}
print(sum(len((r.get("state") or {}).get("events") or []) + len((r.get("timeline") or {}).get("events") or []) for r in rooms.values()))' 2>/dev/null || echo 0)
  check "/sync state+timeline events" "$sync_state" "$([ "${sync_state:-0}" -gt 0 ] && echo 1 || echo 0)"

  # Медиа: байты скачиваются и sha256 совпадает с записанным при импорте; карантин не отдается.
  local media_id want_sum got_sum
  media_id=$(sql "select media_id from media where not blocked and sha256 is not null order by size_bytes desc, media_id limit 1")
  if [ -n "$media_id" ]; then
    want_sum=$(sql "select encode(sha256,'hex') from media where media_id='$media_id'")
    got_sum=$(curl -s -H "Authorization: Bearer $token" "$BASE/_matrix/client/v1/media/download/$SERVER_NAME/$media_id" | sha256sum | cut -d' ' -f1)
    check "media $media_id sha256" "$got_sum" "$([ "$got_sum" = "$want_sum" ] && echo 1 || echo 0)"
  fi
  local quarantined=${ACCEPT_QUARANTINED:-}
  [ -n "$quarantined" ] || quarantined=$(sql "select media_id from media where quarantined_by is not null order by media_id limit 1")
  if [ -n "$quarantined" ]; then
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$BASE/_matrix/client/v1/media/download/$SERVER_NAME/$quarantined")
    check "media в карантине $quarantined не отдается" "$code" "$([ "$code" = 404 ] && echo 1 || echo 0)"
  fi

  # Последовательность подтянута (новый /send получит позицию больше импортированных).
  local seq
  seq=$(sql "select last_value >= (select coalesce(max(stream_ordering),0) from events) from events_stream_ordering_seq")
  check "sequence >= max(stream_ordering)" "$seq" "$([ "$seq" = t ] && echo 1 || echo 0)"
}

# Синтетический профиль (как раньше, но проверки валят прогон).
run_synthetic() {
  local U R E M
  case "$PROFILE" in
    full)  U=100; R=200; E=100000; M=500 ;;
    small) U=5;   R=5;   E=2000;   M=10 ;;
  esac
  SERVER_NAME=localhost
  local dump="$WORK/dump.jsonl" media_in="$WORK/media-in"
  MEDIA_OUT="$WORK/media-out"
  step "профиль $PROFILE: users=$U rooms=$R events=$E media=$M"
  "$BIN/synapse-fixture" -users "$U" -rooms "$R" -events "$E" -media "$M" \
    -out "$dump" -media-dir "$media_in" -server-name localhost 2>>"$LOG"
  say "дамп: $(wc -l <"$dump") строк, $(du -h "$dump" | cut -f1)"

  grep -E '"kind":"(user|room)"' "$dump" >"$WORK/users-rooms.jsonl"
  grep -E '"kind":"membership"' "$dump" >"$WORK/membership.jsonl"
  reset_schema
  step "импорт"
  timed users+rooms "$BIN/syngo-import" -dsn "$DSN" -file "$WORK/users-rooms.jsonl" -server-name localhost -report "$WORK/r1.json"
  timed events "$BIN/syngo-import-events" -dsn "$DSN" -file "$dump" -batch 1000 -progress 0
  timed memberships "$BIN/syngo-import" -dsn "$DSN" -file "$WORK/membership.jsonl" -server-name localhost -report "$WORK/r2.json"
  timed media "$BIN/syngo-import-media" -dsn "$DSN" -dump "$dump" -media-store "$media_in" \
    -object-dir "$MEDIA_OUT" -server-name localhost -report "$WORK/media.txt"
  say "memberships: $(sql 'select count(*) from room_memberships')"

  local topic state_rooms rooms
  topic=$(sql "select coalesce(e.content->>'topic','') from room_state rs join events e on e.event_id=rs.event_id where rs.room_id='!room000:localhost' and rs.type='m.room.topic' limit 1")
  state_rooms=$(sql "select count(distinct room_id) from room_state")
  rooms=$(sql "select count(*) from rooms")
  check "room_state m.room.topic" "${topic:-нет}" "$([ -n "$topic" ] && echo 1 || echo 0)"
  check "room_state комнат" "$state_rooms/$rooms" "$([ "$state_rooms" = "$rooms" ] && echo 1 || echo 0)"

  start_server
  api_checks user000 fixture-pass '!room000:localhost'
  SUMMARY="users=$U rooms=$R events=$E media=$M"
}

# Runbook на БД Synapse (фикстура или копия): шаги 3.1-3.6 и сверка.
run_synapse() {
  : "${SYNAPSE_DSN:?нужен SYNAPSE_DSN}" "${MEDIA_STORE:?нужен MEDIA_STORE}" "${SERVER_NAME:?нужен SERVER_NAME}"
  : "${ACCEPT_USER:?нужен ACCEPT_USER}" "${ACCEPT_PASSWORD:?нужен ACCEPT_PASSWORD}"
  MEDIA_OUT="$WORK/media-out"
  local all="$WORK/all.jsonl" events="$WORK/events.jsonl"

  reset_schema
  if [ "$PROFILE" = fixture ]; then
    psql "$DSN" -v ON_ERROR_STOP=1 -q -f internal/importevents/testdata/synapse-1.165.sql \
      -f internal/importevents/testdata/synapse-1.165-media.sql \
      -f internal/importevents/testdata/synapse-1.165-userdata-schema.sql >>"$LOG" 2>&1
  fi

  step "выгрузка из Synapse (шаг 3.2)"
  timed export.sql psql "$SYNAPSE_DSN" -v ON_ERROR_STOP=1 -At -o "$all" -f tools/synapse-export/export.sql
  timed syngo-synapse-export "$BIN/syngo-synapse-export" -synapse-dsn "$SYNAPSE_DSN" -out "$events"
  say "дамп: $(wc -l <"$all") строк export.sql, $(wc -l <"$events") строк событий"

  step "импорт (шаги 3.3-3.6)"
  grep -E '"kind":"(user|room|room_alias)"' "$all" >"$WORK/users-rooms.jsonl"
  grep -E '"kind":"membership"' "$all" >"$WORK/membership.jsonl"
  grep -E '"kind":"media"' "$all" >"$WORK/media.jsonl" || true
  timed users+rooms "$BIN/syngo-import" -dsn "$DSN" -file "$WORK/users-rooms.jsonl" -server-name "$SERVER_NAME" -report "$WORK/r1.json"
  timed events "$BIN/syngo-import-events" -dsn "$DSN" -file "$events" -batch 1000 -progress 0 \
    -checkpoint "$WORK/events.checkpoint" -bad-file "$WORK/events-bad.jsonl" -report "$WORK/events-import.txt"
  timed memberships "$BIN/syngo-import" -dsn "$DSN" -file "$WORK/membership.jsonl" -server-name "$SERVER_NAME" -report "$WORK/r2.json"
  timed media "$BIN/syngo-import-media" -dsn "$DSN" -dump "$WORK/media.jsonl" -media-store "$MEDIA_STORE" \
    -object-dir "$MEDIA_OUT" -server-name "$SERVER_NAME" -report "$WORK/media-import.txt"
  say "медиа: $(sed -n 's/^\(copied=.*\)$/\1/p' "$WORK/media-import.txt")"

  # Повторный запуск медиа ничего не копирует.
  timed media-rerun "$BIN/syngo-import-media" -dsn "$DSN" -dump "$WORK/media.jsonl" -media-store "$MEDIA_STORE" \
    -object-dir "$MEDIA_OUT" -server-name "$SERVER_NAME" -report "$WORK/media-rerun.txt"
  local rerun
  rerun=$(sed -n 's/^copied=\([0-9]*\) .*/\1/p' "$WORK/media-rerun.txt")
  check "повторный импорт медиа copied" "$rerun" "$([ "$rerun" = 0 ] && echo 1 || echo 0)"

  step "сверка с Synapse (шаг 3.7)"
  if "$BIN/syngo-verify-migration" -synapse-dsn "$SYNAPSE_DSN" -dsn "$DSN" -server-name "$SERVER_NAME" \
    -report "$WORK/verify.txt" >>"$LOG" 2>&1; then
    check "syngo-verify-migration" "$(grep '^result' "$WORK/verify.txt")" 1
  else
    check "syngo-verify-migration" "$(grep -E '^(result|problem)' "$WORK/verify.txt" | head -5 | tr '\n' ' ')" 0
  fi
  sed -n '1,12p' "$WORK/verify.txt" | tee -a "$LOG"

  local room
  if [ -n "${ACCEPT_ROOM_ALIAS:-}" ]; then
    room=$(sql "select room_id from room_aliases where room_alias='$ACCEPT_ROOM_ALIAS'")
  else
    room=$(sql "select room_id from room_memberships where user_id='@$ACCEPT_USER:$SERVER_NAME' and membership='join' order by room_id limit 1")
  fi

  start_server
  api_checks "$ACCEPT_USER" "$ACCEPT_PASSWORD" "$room"
  SUMMARY="users=$(sql 'select count(*) from users') rooms=$(sql 'select count(*) from rooms') events=$(sql 'select count(*) from events')+$(sql 'select count(*) from excluded_events') excluded media=$(sql 'select count(*) from media')"
}

build
case "$PROFILE" in
  small|full) run_synthetic ;;
  fixture)
    SEP='?'; [[ $DSN == *\?* ]] && SEP='&'
    SYNAPSE_DSN="${DSN}${SEP}options=-csearch_path%3Dsynapse_fixture"
    MEDIA_STORE=$PWD/internal/importevents/testdata/synapse-media
    SERVER_NAME=c4.test ACCEPT_USER=alice ACCEPT_PASSWORD=pass-alice ACCEPT_ROOM_ALIAS='#general:c4.test'
    run_synapse ;;
  synapse) run_synapse ;;
  *) echo "неизвестный профиль: $PROFILE" >&2; exit 2 ;;
esac

kill "$SERVER_PID" 2>/dev/null || true
SERVER_PID=""

step "итог"
say "$SUMMARY | ${TIMES[*]}"
if [ -n "$REPORT" ]; then
  {
    echo "# Приёмка миграции ($PROFILE)"
    echo
    echo "- объем: $SUMMARY"
    echo "- время шагов: ${TIMES[*]}"
    for c in "${CHECKS[@]}"; do echo "- $c"; done
    [ -f "$WORK/verify.txt" ] && { echo; echo '```'; cat "$WORK/verify.txt"; echo '```'; }
  } >"$REPORT"
  say "отчёт: $REPORT"
fi
if [ "$FAILED" != 0 ]; then
  say "ПРИЕМКА НЕ ПРОЙДЕНА"
  exit 1
fi
say "приемка пройдена"
