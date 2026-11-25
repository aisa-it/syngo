#!/usr/bin/env bash
# Нагрузочный стенд syngod: отдельный процесс из текущего checkout на
# своих портах и со своей БД, конфиг как в проде (env prod, ops-листенер, лимиты).
#
#   scripts/load-stand.sh up       # собрать, мигрировать, запустить (идемпотентно)
#   scripts/load-stand.sh down     # остановить
#   scripts/load-stand.sh status   # запущен ли, адреса
#   scripts/load-stand.sh reset    # очистить БД и медиа стенда (только остановленного)
#
# Окружение (значения по умолчанию - в скобках):
#   LOAD_DSN         DSN бенч-базы (postgres://syngo:syngo@127.0.0.1:5432/syngo_bench);
#                    имя базы обязано содержать bench или perf
#   LOAD_ADDR        адрес клиентского API (127.0.0.1:38008)
#   LOAD_OPS_ADDR    служебный листенер /metrics (127.0.0.1:39108)
#   LOAD_RATE_LIMIT  prod - лимиты send/media/sync как в проде (config.Default), а
#                    login/register сняты: они нужны только подготовке пула;
#                    off - все лимиты сняты (prod)
#   LOAD_DB_MAX_CONNS  database.max_conns (10, как в проде)
#   LOAD_LOG_LEVEL   уровень лога (info, как в проде)
#   LOAD_PPROF       true - /debug/pprof/ на служебном листенере (false)
#   LOAD_DIR         каталог стенда: бинарь, конфиг, секреты, медиа, лог (var/loadstand)
set -euo pipefail

cd "$(dirname "$0")/.."

LOAD_DSN=${LOAD_DSN:-postgres://syngo:syngo@127.0.0.1:5432/syngo_bench?sslmode=disable}
LOAD_ADDR=${LOAD_ADDR:-127.0.0.1:38008}
LOAD_OPS_ADDR=${LOAD_OPS_ADDR:-127.0.0.1:39108}
LOAD_RATE_LIMIT=${LOAD_RATE_LIMIT:-prod}
LOAD_DB_MAX_CONNS=${LOAD_DB_MAX_CONNS:-10}
LOAD_LOG_LEVEL=${LOAD_LOG_LEVEL:-info}
LOAD_PPROF=${LOAD_PPROF:-false}
LOAD_DIR=${LOAD_DIR:-var/loadstand}
LOAD_SERVER_NAME=${LOAD_SERVER_NAME:-bench.loadtest}

PID_FILE=$LOAD_DIR/syngod.pid
STATE=$LOAD_DIR/stand.env
SECRETS=$LOAD_DIR/secrets.env

die() { echo "load-stand: $*" >&2; exit 1; }

db_name() { # имя базы из DSN (URL или key=value)
	local dsn=$1
	if [[ $dsn == *://* ]]; then
		dsn=${dsn#*://}; dsn=${dsn#*/}; echo "${dsn%%\?*}"
	else
		sed -n 's/.*dbname=\([^ ]*\).*/\1/p' <<<"$dsn"
	fi
}

guard_db() {
	local name
	name=$(db_name "$LOAD_DSN")
	[[ $name == *bench* || $name == *perf* ]] || die "база '$name' не похожа на бенч-базу (bench/perf в имени)"
}

port_of() { echo "${1##*:}"; }

guard_ports() {
	local p
	for p in $(port_of "$LOAD_ADDR") $(port_of "$LOAD_OPS_ADDR"); do
		if ss -ltn "sport = :$p" 2>/dev/null | grep -q LISTEN; then
			die "порт $p уже слушается"
		fi
	done
}

running() { [[ -f $PID_FILE ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; }

secrets() {
	if [[ ! -f $SECRETS ]]; then
		umask 077
		{
			echo "SYNGOD_SYNC_TOKEN_SECRET=$(openssl rand -hex 32)"
			echo "SYNGOD_REGISTRATION_SHARED_SECRET=$(openssl rand -hex 32)"
		} >"$SECRETS"
	fi
	# shellcheck disable=SC1090
	source "$SECRETS"
}

rate_limit_yaml() {
	case $LOAD_RATE_LIMIT in
	prod)
		# send/media_upload/sync - значения config.Default (как в проде, deploy/prod/config.yaml
		# их не переопределяет); login/register - только подготовка пула, сняты.
		cat <<-'EOF'
		rate_limit:
		  login_ip:      { enabled: false }
		  login_account: { enabled: false }
		  register_ip:   { enabled: false }
		  send:          { enabled: true, per_second: 0.2, burst: 10 }
		  media_upload:  { enabled: true, per_second: 10,  burst: 50 }
		  sync:          { enabled: true, per_second: 2,   burst: 20 }
		EOF
		;;
	off)
		cat <<-'EOF'
		rate_limit:
		  login_ip:      { enabled: false }
		  login_account: { enabled: false }
		  register_ip:   { enabled: false }
		  send:          { enabled: false }
		  media_upload:  { enabled: false }
		  sync:          { enabled: false }
		EOF
		;;
	*) die "LOAD_RATE_LIMIT=$LOAD_RATE_LIMIT: prod или off" ;;
	esac
}

write_config() {
	# Основа - deploy/prod/config.yaml: env prod, таймауты, медиа, федерация выключена.
	cat >"$LOAD_DIR/config.yaml" <<EOF
# Сгенерировано scripts/load-stand.sh: конфиг нагрузочного стенда,
# близкий к deploy/prod/config.yaml. Секреты - в окружении ($SECRETS).
env: prod
server:
  addr: "$LOAD_ADDR"
  read_header_timeout: 10s
  read_timeout: 30s
  write_timeout: 60s
  idle_timeout: 120s
  shutdown_timeout: 15s
  ops_addr: "$LOAD_OPS_ADDR"
  pprof: $LOAD_PPROF
  trusted_proxies: []
matrix:
  server_name: "$LOAD_SERVER_NAME"
database:
  max_conns: $LOAD_DB_MAX_CONNS
  connect_timeout: 5s
sync:
  max_timeout_ms: 30000
  per_room_limit: 20
auth:
  registration_open: false
  min_password_length: 8
media:
  storage_backend: local
  directory: "$PWD/$LOAD_DIR/media"
  max_upload_bytes: 52428800
log:
  level: $LOAD_LOG_LEVEL
  format: json
federation:
  enabled: false
$(rate_limit_yaml)
EOF
}

write_state() {
	cat >"$STATE" <<EOF
LOAD_TARGET=http://$LOAD_ADDR
LOAD_METRICS_URL=http://$LOAD_OPS_ADDR/metrics
LOAD_PID=$(cat "$PID_FILE")
LOAD_DSN='$LOAD_DSN'
LOAD_RATE_LIMIT=$LOAD_RATE_LIMIT
LOAD_DB_MAX_CONNS=$LOAD_DB_MAX_CONNS
LOAD_LOG_LEVEL=$LOAD_LOG_LEVEL
LOAD_SERVER_NAME=$LOAD_SERVER_NAME
LOAD_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)$(git diff --quiet 2>/dev/null || echo -dirty)
EOF
}

cmd_up() {
	guard_db
	mkdir -p "$LOAD_DIR/media"
	if running; then
		echo "load-stand: уже запущен (pid $(cat "$PID_FILE"))"
		return 0
	fi
	guard_ports
	secrets

	local version commit
	version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
	commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
	echo "== сборка syngod ($version)"
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w \
		-X github.com/aisa-it/syngo/internal/build.Version=$version \
		-X github.com/aisa-it/syngo/internal/build.Commit=$commit" \
		-o "$LOAD_DIR/syngod" ./cmd/syngod

	echo "== миграции ($(db_name "$LOAD_DSN"))"
	go run ./cmd/migrate -dsn "$LOAD_DSN" -up

	write_config
	echo "== запуск syngod: $LOAD_ADDR, ops $LOAD_OPS_ADDR, rate_limit=$LOAD_RATE_LIMIT, max_conns=$LOAD_DB_MAX_CONNS"
	: >"$LOAD_DIR/syngod.log"
	SYNGOD_CONFIG="$LOAD_DIR/config.yaml" SYNGOD_DATABASE_DSN="$LOAD_DSN" \
		SYNGOD_SYNC_TOKEN_SECRET="$SYNGOD_SYNC_TOKEN_SECRET" \
		SYNGOD_REGISTRATION_SHARED_SECRET="$SYNGOD_REGISTRATION_SHARED_SECRET" \
		nohup "$LOAD_DIR/syngod" >>"$LOAD_DIR/syngod.log" 2>&1 </dev/null &
	echo $! >"$PID_FILE"

	local i
	for i in $(seq 1 60); do
		if curl -fsS "http://$LOAD_OPS_ADDR/health/ready" >/dev/null 2>&1; then
			write_state
			echo "load-stand: готов (pid $(cat "$PID_FILE")), состояние - $STATE"
			return 0
		fi
		running || { tail -20 "$LOAD_DIR/syngod.log" >&2; die "syngod завершился при старте"; }
		sleep 0.5
	done
	tail -20 "$LOAD_DIR/syngod.log" >&2
	die "syngod не стал ready за 30 с"
}

cmd_down() {
	if ! running; then
		echo "load-stand: не запущен"
		rm -f "$PID_FILE"
		return 0
	fi
	local pid i
	pid=$(cat "$PID_FILE")
	kill -TERM "$pid"
	for i in $(seq 1 40); do
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.5
	done
	kill -0 "$pid" 2>/dev/null && kill -KILL "$pid"
	rm -f "$PID_FILE"
	echo "load-stand: остановлен"
}

cmd_status() {
	if running; then
		echo "load-stand: запущен (pid $(cat "$PID_FILE"))"
		cat "$STATE"
	else
		echo "load-stand: не запущен"
		return 3
	fi
}

cmd_reset() {
	guard_db
	running && die "сначала down"
	echo "== очистка схемы $(db_name "$LOAD_DSN")"
	psql "$LOAD_DSN" -v ON_ERROR_STOP=1 -q -c 'SET client_min_messages = warning; DROP SCHEMA public CASCADE; CREATE SCHEMA public;'
	rm -rf "$LOAD_DIR/media" var/loadtest/tokens.json
	echo "load-stand: база и медиа очищены (миграции - при следующем up)"
}

case ${1:-} in
up) cmd_up ;;
down) cmd_down ;;
status) cmd_status ;;
reset) cmd_reset ;;
*) echo "использование: $0 up|down|status|reset" >&2; exit 2 ;;
esac
