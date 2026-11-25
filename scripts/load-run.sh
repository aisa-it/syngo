#!/usr/bin/env bash
# Прогон cmd/loadgen против нагрузочного стенда.
#
#   scripts/load-run.sh [syngo|synapse] [профиль] [флаги loadgen...]
#
#   scripts/load-run.sh syngo S                 # smoke на стенде scripts/load-stand.sh
#   scripts/load-run.sh syngo M -duration 5m    # профиль M с коротким окном
#   scripts/load-run.sh synapse S               # тот же сценарий против Synapse (scripts/load-synapse.sh)
#
# Адреса, PID, DSN и секрет регистрации берутся из состояния стенда
# (var/loadstand/stand.env или var/loadsynapse/stand.env). Отчеты - var/loadtest/.
set -euo pipefail

cd "$(dirname "$0")/.."

kind=${1:-syngo}
profile=${2:-S}
shift $(( $# >= 2 ? 2 : $# ))

case $kind in
syngo) dir=${LOAD_DIR:-var/loadstand} ;;
synapse) dir=${LOAD_SYNAPSE_DIR:-var/loadsynapse} ;;
*) echo "load-run: сервер $kind: syngo или synapse" >&2; exit 2 ;;
esac

[[ -f $dir/stand.env && -f $dir/secrets.env ]] || {
	echo "load-run: стенд $kind не поднят ($dir/stand.env): make load-stand-up / make load-synapse-up" >&2
	exit 1
}

# shellcheck disable=SC1091
source "$dir/stand.env"
# shellcheck disable=SC1091
source "$dir/secrets.env"

if [[ $kind == syngo ]] && ! kill -0 "$LOAD_PID" 2>/dev/null; then
	echo "load-run: syngod стенда (pid $LOAD_PID) не запущен: make load-stand-up" >&2
	exit 1
fi

profile_args=(-profile "$profile")
[[ -f $profile ]] && profile_args=(-profile-file "$profile")

export LOADGEN_SHARED_SECRET=${SYNGOD_REGISTRATION_SHARED_SECRET:-${SYNAPSE_REGISTRATION_SHARED_SECRET:-}}
export LOADGEN_PG_DSN=$LOAD_DSN

# Пользователи у каждого сервера свои: кеш токенов - по адресу сервера внутри файла.
exec go run ./cmd/loadgen \
	-target "$LOAD_TARGET" -server-kind "$kind" -metrics-url "$LOAD_METRICS_URL" -server-pid "$LOAD_PID" \
	-commit "$(git rev-parse --short HEAD 2>/dev/null || echo unknown)" \
	-label "server_commit=$LOAD_COMMIT" -label "rate_limit=$LOAD_RATE_LIMIT" \
	-label "db_max_conns=$LOAD_DB_MAX_CONNS" -label "log_level=$LOAD_LOG_LEVEL" \
	-label "server_name=$LOAD_SERVER_NAME" \
	"${profile_args[@]}" "$@"
