#!/usr/bin/env bash
# Perf-ratchet для /sync: гоняет cmd/syncload (слой данных и
# HTTP-обработчик /sync) и падает при регрессии относительно bench/sync-baseline.json.
#
#   PSQL_DSN=... scripts/perf-ratchet.sh            # проверка на профиле baseline
#   PSQL_DSN=... scripts/perf-ratchet.sh --update   # снять baseline (профиль: ROOMS, EVENTS, ...)
#
# База - чистая схема (после migrate up, без данных прошлых прогонов).
#
# В режиме проверки профиль берется из baseline: прогон на другом профиле
# сравнивать не с чем. Пороги и правила - tools/ratchet/perf_ratchet.py (константы,
# окружением не ослабляются). Метрики прогона: $METRICS (артефакт CI).
set -euo pipefail

cd "$(dirname "$0")/.."

BASELINE=bench/sync-baseline.json
METRICS=${METRICS:-/tmp/sync-metrics.json}
RATCHET=tools/ratchet/perf_ratchet.py
update=0
[[ ${1:-} == "--update" ]] && update=1

if [[ -z ${PSQL_DSN:-} ]]; then
    echo "perf-ratchet: нужен PSQL_DSN" >&2
    exit 1
fi

if [[ $update -eq 1 ]]; then
    PERF_ROOMS=${ROOMS:-500} PERF_EVENTS_PER_ROOM=${EVENTS:-10} PERF_MEMBERS_PER_ROOM=${MEMBERS:-5}
    PERF_USERS=${USERS:-10} PERF_CLIENTS=${CLIENTS:-10} PERF_ITERATIONS=${ITERATIONS:-30}
    PERF_PER_ROOM_LIMIT=${PER_ROOM_LIMIT:-20}
else
    [[ -f $BASELINE ]] || { echo "perf-ratchet: нет baseline ($BASELINE), снимите: --update" >&2; exit 1; }
    profile=$(python3 "$RATCHET" profile --baseline "$BASELINE")
    eval "$profile"
fi

# Отчет в markdown - во временный каталог: версионируемые bench/sync-*.md
# прогон ratchet не перезаписывает.
report_dir=$(mktemp -d)
trap 'rm -rf "$report_dir"' EXIT

echo "== syncload: ${PERF_ROOMS} комнат × ${PERF_EVENTS_PER_ROOM} событий, ${PERF_MEMBERS_PER_ROOM} участников, ${PERF_CLIENTS} клиентов × ${PERF_ITERATIONS}"
# Ошибка сборки /sync - ненулевой код syncload, set -e роняет гейт.
# -require-empty: число SQL-запросов зависит от формы данных, сравниваем только
# прогон на чистой схеме (в CI база сервиса всегда новая).
go run ./cmd/syncload -dsn "$PSQL_DSN" -scenario all -require-empty \
    -rooms "$PERF_ROOMS" -events-per-room "$PERF_EVENTS_PER_ROOM" -members-per-room "$PERF_MEMBERS_PER_ROOM" \
    -users "$PERF_USERS" -clients "$PERF_CLIENTS" -iterations "$PERF_ITERATIONS" \
    -per-room-limit "$PERF_PER_ROOM_LIMIT" -out-dir "$report_dir" -json "$METRICS"

if [[ $update -eq 1 ]]; then
    exec python3 "$RATCHET" update --baseline "$BASELINE" --current "$METRICS"
fi

exec python3 "$RATCHET" check --baseline "$BASELINE" --current "$METRICS"
