#!/usr/bin/env bash
# Synapse для сравнения под нагрузкой cmd/loadgen:
# deploy/loadtest/docker-compose.synapse.yml со своей PostgreSQL.
#
#   scripts/load-synapse.sh up       # отрендерить конфиг и поднять
#   scripts/load-synapse.sh down     # остановить (тома сохраняются)
#   scripts/load-synapse.sh status
#   scripts/load-synapse.sh reset    # снести вместе с томами (база, медиа, ключ)
#
# Окружение:
#   LOAD_RATE_LIMIT  prod - лимиты Synapse по умолчанию (rc_message 0.2/с, burst 10 и
#                    т.д.), сняты только rc_login, rc_registration и rc_joins: они нужны
#                    подготовке пула (у syngod лимита на join нет); off - все сняты
#   LOAD_SYNAPSE_DIR каталог состояния (var/loadsynapse)
set -euo pipefail

cd "$(dirname "$0")/.."

LOAD_RATE_LIMIT=${LOAD_RATE_LIMIT:-prod}
LOAD_SYNAPSE_DIR=${LOAD_SYNAPSE_DIR:-var/loadsynapse}
LOAD_SERVER_NAME=${LOAD_SERVER_NAME:-bench.synapse}
COMPOSE_FILE=deploy/loadtest/docker-compose.synapse.yml
CONF=$PWD/$LOAD_SYNAPSE_DIR/conf
SECRETS=$LOAD_SYNAPSE_DIR/secrets.env
STATE=$LOAD_SYNAPSE_DIR/stand.env

export LOAD_SYNAPSE_CONF=$CONF

die() { echo "load-synapse: $*" >&2; exit 1; }
compose() { docker compose -f "$COMPOSE_FILE" "$@"; }

relaxed() { # relaxed <ключ rc_*> - лимит, снятый для подготовки
	printf '%s:\n  per_second: 1000\n  burst_count: 1000\n' "$1"
}

rate_limits() {
	case $LOAD_RATE_LIMIT in
	prod)
		cat <<-'EOF'
		rc_login:
		  address: {per_second: 1000, burst_count: 1000}
		  account: {per_second: 1000, burst_count: 1000}
		  failed_attempts: {per_second: 1000, burst_count: 1000}
		rc_registration: {per_second: 1000, burst_count: 1000}
		rc_joins:
		  local: {per_second: 1000, burst_count: 1000}
		  remote: {per_second: 1000, burst_count: 1000}
		EOF
		;;
	off)
		cat <<-'EOF'
		rc_login:
		  address: {per_second: 1000, burst_count: 1000}
		  account: {per_second: 1000, burst_count: 1000}
		  failed_attempts: {per_second: 1000, burst_count: 1000}
		rc_joins:
		  local: {per_second: 1000, burst_count: 1000}
		  remote: {per_second: 1000, burst_count: 1000}
		rc_invites:
		  per_room: {per_second: 1000, burst_count: 1000}
		  per_user: {per_second: 1000, burst_count: 1000}
		  per_issuer: {per_second: 1000, burst_count: 1000}
		rc_media_create: {per_second: 1000, burst_count: 1000}
		rc_room_creation: {per_second: 1000, burst_count: 1000}
		EOF
		relaxed rc_registration
		relaxed rc_message
		;;
	*) die "LOAD_RATE_LIMIT=$LOAD_RATE_LIMIT: prod или off" ;;
	esac
}

render() {
	mkdir -p "$CONF"
	if [[ ! -f $SECRETS ]]; then
		umask 077
		echo "SYNAPSE_REGISTRATION_SHARED_SECRET=$(openssl rand -hex 32)" >"$SECRETS"
	fi
	# shellcheck disable=SC1090
	source "$SECRETS"

	cat >"$CONF/homeserver.yaml" <<EOF
# Сгенерировано scripts/load-synapse.sh. Монолит, настройки по
# умолчанию, кроме лимитов подготовки (LOAD_RATE_LIMIT=$LOAD_RATE_LIMIT).
server_name: $LOAD_SERVER_NAME
public_baseurl: http://127.0.0.1:38018/
pid_file: /data/homeserver.pid
listeners:
  - port: 8008
    tls: false
    type: http
    x_forwarded: false
    bind_addresses: ["0.0.0.0"]
    resources:
      - names: [client]
        compress: false
  - port: 9000
    type: metrics
    bind_addresses: ["0.0.0.0"]
enable_metrics: true
database:
  name: psycopg2
  args:
    user: synapse
    password: synapse-bench
    database: synapse_bench
    host: synapse-pg
    port: 5432
    cp_min: 5
    cp_max: 10
media_store_path: /media
signing_key_path: /data/synapse.signing.key
trusted_key_servers: []
suppress_key_server_warning: true
federation_domain_whitelist: []
registration_shared_secret: "$SYNAPSE_REGISTRATION_SHARED_SECRET"
enable_registration: false
allow_guest_access: false
report_stats: false
log_config: /data/log.config
$(rate_limits)
EOF
	cat >"$CONF/log.config" <<'EOF'
version: 1
formatters:
  simple:
    format: "%(asctime)s - %(name)s - %(levelname)s - %(message)s"
handlers:
  console:
    class: logging.StreamHandler
    formatter: simple
root:
  level: WARNING
  handlers: [console]
disable_existing_loggers: false
EOF
}

cmd_up() {
	render
	compose up -d
	local i
	for i in $(seq 1 90); do
		if curl -fsS http://127.0.0.1:38018/health >/dev/null 2>&1; then
			local pid
			pid=$(docker inspect -f '{{.State.Pid}}' syngo-load-synapse)
			cat >"$STATE" <<EOF
LOAD_TARGET=http://127.0.0.1:38018
LOAD_METRICS_URL=http://127.0.0.1:39118/_synapse/metrics
LOAD_PID=$pid
LOAD_DSN='postgres://synapse:synapse-bench@127.0.0.1:35432/synapse_bench?sslmode=disable'
LOAD_RATE_LIMIT=$LOAD_RATE_LIMIT
LOAD_DB_MAX_CONNS=10
LOAD_LOG_LEVEL=warning
LOAD_SERVER_NAME=$LOAD_SERVER_NAME
LOAD_COMMIT=synapse-v1.165.0
EOF
			echo "load-synapse: готов (pid $pid), состояние - $STATE"
			return 0
		fi
		sleep 1
	done
	compose logs --tail=30 synapse >&2
	die "Synapse не ответил на /health за 90 с"
}

case ${1:-} in
up) cmd_up ;;
down) compose down; rm -f "$STATE" ;;
status) compose ps ;;
reset) compose down -v; rm -rf "$LOAD_SYNAPSE_DIR" ;;
*) echo "использование: $0 up|down|status|reset" >&2; exit 2 ;;
esac
