#!/usr/bin/env bash
# Интероп-прогон с живым Synapse: сценарии федерации и сверка room_state 1:1.
#
#   deploy/stand/interop.sh            # весь прогон + отчёт в docs/interop-report.md
#   deploy/stand/interop.sh --keep     # не гасить стенд после прогона
#
# Требования: docker compose v2, curl, python3, openssl. Наш бинарь syngo-interop
# собирается в контейнере (образ стенда) и запускается как one-shot сервис.
set -euo pipefail

cd "$(dirname "$0")"

# KEEP берём из окружения тоже: удобно для разбора (KEEP=1 bash interop.sh).
KEEP=${KEEP:-0}
[[ ${1:-} == "--keep" ]] && KEEP=1

STATE_FILE=.stand-state.env
INTEROP_STATE=.interop-state.env
REPORT_JSON=${REPORT_JSON:-interop-state.json}
REPORT_MD=../../docs/interop-report.md
# Synapse работает в host-сети: федеративный порт 8448 теперь на хосте.
SYNAPSE_URL=${SYNAPSE_URL:-https://127.0.0.1:8448}
# Адрес Synapse из контейнера сверки (он в docker-сети, Synapse — в host-сети).
SYNAPSE_INTERNAL_URL=${SYNAPSE_INTERNAL_URL:-https://synapse.test:8448}
SYNAPSE_SECRET=stand-registration-secret
SYNAPSE_ADMIN=interop-admin
SYNAPSE_ADMIN_PASSWORD=interop-admin-password
# Пароль тестового пользователя Synapse (bob@synapse.test); нужен для /login после регистрации.
BOB_PASSWORD=${BOB_PASSWORD:-interop-bob-password}
A_CONTAINER=syngo-stand-syngod-a-1

compose() { docker compose -f docker-compose.yml -f docker-compose.synapse.yml "$@"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
log() { printf '%s\n' "==> $*"; }
json_field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1],""))' "$1"; }

command -v docker >/dev/null || fail "нужен docker"
docker compose version >/dev/null || fail "нужен docker compose v2"

# Synapse (Twisted) не смотрит в /etc/hosts своего контейнера, поэтому живёт в host-сети
# и резолвит участников стенда через /etc/hosts ХОСТА. Записи добавляем сами.
set_host_entry() { # set_host_entry <name> <ip>: заменяет прежние записи (иначе getent отдаёт старый IP)
  local name=$1 ip=${2:-127.0.0.1}
  local esc=${name//./\\.}
  local pattern="[[:space:]]${esc}$"

  if [[ $(getent hosts "$name" | grep -cw "$ip") -eq 1 && $(getent hosts "$name" | wc -l) -eq 1 ]]; then
    return 0
  fi

  if [[ -w /etc/hosts ]]; then
    sed -i "/${pattern}/d" /etc/hosts
    printf '%s %s\n' "$ip" "$name" >> /etc/hosts
    log "обновил /etc/hosts: $name → $ip"
  elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
    sudo sed -i "/${pattern}/d" /etc/hosts
    printf '%s %s\n' "$ip" "$name" | sudo tee -a /etc/hosts >/dev/null
    log "обновил /etc/hosts через sudo: $name → $ip"
  else
    fail "нужна запись '$name → $ip' в /etc/hosts хоста (нет прав, добавьте вручную)"
  fi
}

set_host_entry a.test 127.0.0.2
set_host_entry b.test 127.0.0.2
set_host_entry synapse.test 127.0.0.1

# Preflight после подъёма: проверяет каждый хоп федерации и печатает вердикт
# (hosts, TLS до Synapse/a.test, ключи a.test из контейнера Synapse).
run_preflight() {
  log "preflight федерации"
  ./preflight.sh || fail "preflight не прошёл: смотрите провалы выше"

  # Условие интеропа: проверка подписей входящих событий обязана быть
  # включена. Если в конфиге стенда появился skip — приёмка недействительна.
  if grep -qE '^[[:space:]]*insecure_skip_signature_verify:[[:space:]]*true' config-a-interop.yaml; then
    fail "в config-a-interop.yaml включён insecure_skip_signature_verify: интероп принимается только с проверкой подписей"
  fi
}

# 1. Стенд SYNGO (если ещё не поднят) + сертификаты + Synapse.
[[ -f .env ]] || cp .env.example .env
chmod 600 .env

# Пароль БД берём из .env: compose подставляет его же в POSTGRES_PASSWORD,
# иначе сверка падала с FATAL: password authentication failed.
set -a
# shellcheck disable=SC1091
source .env
set +a
[[ -f $STATE_FILE ]] || ./bootstrap.sh >/dev/null

log "генерирую сертификаты стенда"
./interop/certs.sh >/dev/null

# Synapse читает свои сертификаты как uid 991, а приватный ключ стенда — 600.
# Named volume /data готовит entrypoint overlay, поэтому права на каталог данных
# и chown вручную не нужны.
# Каталог certs создаётся с 700: контейнер Synapse (uid 991) не может даже зайти
# в него. Для стенда открываем каталог и файлы на чтение.
chmod 755 certs
chmod 644 certs/*.crt
chmod 644 certs/synapse.test.key
# Наш syngod в stand-образе работает под uid 10001: ключ a.test должен быть читаем.
chmod 644 certs/a.test.key


log "поднимаю Synapse $SYNAPSE_URL"
DB_STARTED=$(docker compose ps -q postgres || true)
[[ -n $DB_STARTED ]] || compose up -d postgres
sleep 2
# Образ пересобираем из текущего checkout: иначе стенд гоняет старый бинарь и «баги» не воспроизводятся.
compose build syngod-a >/dev/null
compose up -d syngod-a synapse

# Synapse в host-сети слушает 8448 напрямую, поэтому готовность проверяем по https
# с -k: сертификат самоподписанный (стенд).
wait_http() {
	local url=$1 name=$2 i
	for i in $(seq 1 90); do
		if curl -fskS --max-time 2 "$url" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	fail "$name не ответил за 90 секунд ($url)"
}

wait_http "$SYNAPSE_URL/_matrix/client/versions" synapse
A_URL=${A_URL:-http://127.0.0.1:18008}
# В interop-режиме a.test слушает HTTPS на том же порту.
wait_http_https() {
	local i
	for i in $(seq 1 90); do
		if curl -kfsS --max-time 2 "$1/health" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	fail "syngod-a (TLS) не ответил за 90 секунд"
}
wait_http_https "https://127.0.0.1:18008"
A_URL=https://127.0.0.1:18008

# Стенд поднят — проверяем хопы федерации до сценариев.
run_preflight

source "$STATE_FILE"

# STATE_FILE пишет bootstrap и хранит http-адреса: в interop-режиме a.test слушает TLS,
# поэтому принудительно переводим адрес на https.
A_URL=https://127.0.0.1:18008

# 2. Пользователи Synapse: admin + bob (через shared secret registration).
register_synapse() { # register_synapse <username> <password> <admin:true|false>
	# Используем официальный CLI из образа Synapse: он сам считает HMAC по shared secret
	# (ручная реализация на python давала «HMAC incorrect»).
	local admin_flag=--no-admin
	[[ $3 == "true" ]] && admin_flag=--admin

	compose -f docker-compose.yml -f docker-compose.synapse.yml exec -T synapse \
		register_new_matrix_user -c /data/homeserver.yaml -u "$1" -p "$2" "$admin_flag" \
		http://127.0.0.1:8008 >/dev/null 2>&1 || true
}

login_synapse() { # login_synapse <username> <password>
	curl -sSk --max-time 15 -X POST "$SYNAPSE_URL/_matrix/client/v3/login" \
		-H 'Content-Type: application/json' \
		-d "{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"$1\"},\"password\":\"$2\"}" |
		json_field access_token
}

log "регистрирую пользователей Synapse"
ADMIN_TOKEN=$(register_synapse "$SYNAPSE_ADMIN" "$SYNAPSE_ADMIN_PASSWORD" true)
[[ -n $ADMIN_TOKEN ]] || ADMIN_TOKEN=$(login_synapse "$SYNAPSE_ADMIN" "$SYNAPSE_ADMIN_PASSWORD")
[[ -n $ADMIN_TOKEN ]] || fail "не удалось получить admin-токен Synapse"

SYN_BOB_TOKEN=$(register_synapse bob "$BOB_PASSWORD" false)
[[ -n $SYN_BOB_TOKEN ]] || SYN_BOB_TOKEN=$(login_synapse bob "$BOB_PASSWORD")
[[ -n $SYN_BOB_TOKEN ]] || fail "не удалось получить токен bob@synapse.test"

log "admin Synapse: ${ADMIN_TOKEN:0:6}..., bob: ${SYN_BOB_TOKEN:0:6}..."

umask 077
cat > "$INTEROP_STATE" <<STATE
SYNAPSE_URL=$SYNAPSE_URL
ADMIN_TOKEN=$ADMIN_TOKEN
SYN_BOB_TOKEN=$SYN_BOB_TOKEN
STATE
chmod 600 "$INTEROP_STATE"

# 3. Сравнение состояния: наш room_state (БД) против admin API Synapse.
compare() { # compare <room_id> <label>
	local room=$1 label=$2
	log "сверка состояния: $label ($room)"
	compose --profile tools run --rm --no-deps -T --build \
		--entrypoint /usr/local/bin/syngo-interop interop \
		-dsn "postgres://syngo:${POSTGRES_PASSWORD:-syngo}@postgres:5432/syngo_a?sslmode=disable" \
		-room "$room" -synapse "${SYNAPSE_INTERNAL_URL:-$SYNAPSE_URL}" -token "$ADMIN_TOKEN" \
		-timeout 90s \
		-out "/tmp/interop-$(printf '%s' "$label" | tr '/ ' '__').json"
}

RESULT=0
SKIPPED=()
RESULTS=()

run_scenario() { # run_scenario <name> <function>
	local name=$1 fn=$2
	log "сценарий: $name"
	if "$fn"; then
		RESULTS+=("$name|PASS")
	else
		printf 'FAIL: сценарий %s\n' "$name" >&2
		RESULTS+=("$name|FAIL")
		RESULT=1
	fi
}

skip_scenario() { # skip_scenario <name> <причина>
	SKIPPED+=("$1 — $2")
	log "SKIP: $1 ($2)"
}

# supports_client_endpoint проверяет, реализован ли маршрут клиентского API:
# chi отвечает 405 на известный путь с другим методом и 404 на отсутствующий.
supports_client_endpoint() { # supports_client_endpoint <url> <method>
	local url=$1 method=$2 code
	code=$(curl -ks -o /dev/null -w '%{http_code}' -X "$method" "$url" || true)
	[[ $code != "404" ]]
}

# --- Сценарии -----------------------------------------------------------------------

scenario_invite_join() {
	# a.test приглашает bob@synapse.test, тот вступает через федерацию.
	local body room
	body=$(curl -sS -k --max-time 15 -X POST "$A_URL/_matrix/client/v3/createRoom" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
		-d '{"name":"interop","preset":"private_chat","room_version":"10","invite":["@bob:synapse.test"]}')
	room=$(printf '%s' "$body" | json_field room_id)
	[[ -n $room ]] || { echo "createRoom: $body" >&2; return 1; }
	echo "$room" > .interop-room
	ROOM_ID=$room

	local join
	join=$(curl -sSk --max-time 30 -X POST "$SYNAPSE_URL/_matrix/client/v3/rooms/$room/join" \
		-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' -d '{}')
	printf '%s' "$join" | grep -q '"room_id"' || { echo "join: $join" >&2; return 1; }
	compare "$room" "после invite/join a→synapse"
}

scenario_reverse_join() {
	# Synapse приглашает alice@a.test, наш сервер делает исходящий join.
	local body room join
	body=$(curl -sSk --max-time 15 -X POST "$SYNAPSE_URL/_matrix/client/v3/createRoom" \
		-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' \
		-d '{"name":"interop-reverse","preset":"private_chat","room_version":"10","invite":["@alice:a.test"]}')
	room=$(printf '%s' "$body" | json_field room_id)
	[[ -n $room ]] || { echo "createRoom на Synapse: $body" >&2; return 1; }
	echo "$room" > .interop-room-reverse

	join=$(curl -sSk --max-time 60 -X POST "$A_URL/_matrix/client/v3/rooms/$room/join" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' -d '{}')
	printf '%s' "$join" | grep -q '"room_id"' || { echo "исходящий join: $join" >&2; return 1; }
	sleep 2
	compare "$room" "после invite/join synapse→a"
}

scenario_messages() {
	local room=$ROOM_ID i eid
	for i in $(seq 1 10); do
		eid=$(curl -sSk --max-time 15 -X PUT \
			"$A_URL/_matrix/client/v3/rooms/$room/send/m.room.message/interop-$i" \
			-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
			-d "{\"msgtype\":\"m.text\",\"body\":\"interop-$i\"}" | json_field event_id)
		[[ -n $eid ]] || { echo "send interop-$i" >&2; return 1; }
	done
	sleep 3
	compare "$room" "после 10 сообщений"
}

scenario_leave_rejoin() {
	local room=$ROOM_ID
	curl -sSk --max-time 15 -X POST "$SYNAPSE_URL/_matrix/client/v3/rooms/$room/leave" \
		-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' -d '{}' >/dev/null
	sleep 2
	compare "$room" "после leave"

	curl -sSk --max-time 30 -X POST "$SYNAPSE_URL/_matrix/client/v3/rooms/$room/join" \
		-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' -d '{}' >/dev/null
	sleep 2
	compare "$room" "после rejoin"
}

scenario_pl_conflict() {
	local room=$ROOM_ID
	# Красный/зелёный power-level: alice поднимает bob до 50 на a.test, затем оба
	# одновременно пишут power_levels (bob — на Synapse).
	curl -sSk --max-time 15 -X PUT \
		"$A_URL/_matrix/client/v3/rooms/$room/state/m.room.power_levels/" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
		-d '{"users":{"@alice:a.test":100,"@bob:synapse.test":50}}' >/dev/null

	curl -sSk --max-time 15 -X PUT \
		"$SYNAPSE_URL/_matrix/client/v3/rooms/$room/state/m.room.power_levels/" \
		-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' \
		-d '{"users":{"@alice:a.test":90,"@bob:synapse.test":50}}' >/dev/null

	sleep 3
	compare "$room" "после конфликта power_levels"
}

scenario_ban_unban() {
	local room=$ROOM_ID
	curl -sSk --max-time 15 -X POST "$A_URL/_matrix/client/v3/rooms/$room/ban" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
		-d '{"user_id":"@bob:synapse.test","reason":"interop"}' >/dev/null
	sleep 2
	compare "$room" "после ban"

	curl -sSk --max-time 15 -X POST "$A_URL/_matrix/client/v3/rooms/$room/unban" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
		-d '{"user_id":"@bob:synapse.test"}' >/dev/null
	sleep 2
	compare "$room" "после unban"
}

scenario_backfill() {
	local room=$ROOM_ID i
	log "backfill: остановка syngod-a, события в Synapse, старт и сверка"
	compose stop syngod-a >/dev/null

	for i in $(seq 1 5); do
		curl -sSk --max-time 15 -X PUT \
			"$SYNAPSE_URL/_matrix/client/v3/rooms/$room/send/m.room.message/backfill-$i" \
			-H "Authorization: Bearer $SYN_BOB_TOKEN" -H 'Content-Type: application/json' \
			-d "{\"msgtype\":\"m.text\",\"body\":\"backfill-$i\"}" >/dev/null
	done

	compose start syngod-a >/dev/null
	wait_http_https "https://127.0.0.1:18008"
	sleep 6
	compare "$room" "после простоя и догона (backfill)"
}

run_scenario "invite/join a.test → synapse.test" scenario_invite_join
[[ -n ${ROOM_ID:-} ]] || fail "комната не создана"
run_scenario "invite/join synapse.test → a.test" scenario_reverse_join
run_scenario "10 сообщений и порядок" scenario_messages
run_scenario "конфликт power_levels (красный/зелёный)" scenario_pl_conflict
run_scenario "leave/rejoin" scenario_leave_rejoin
run_scenario "ban/unban" scenario_ban_unban
run_scenario "backfill после простоя" scenario_backfill

# 4. Отчёт.
VERDICT="расхождения есть"
[[ $RESULT -eq 0 ]] && VERDICT="состояния совпадают 1:1"

SYNAPSE_VERSION=$(curl -sS --max-time 5 "$SYNAPSE_URL/_matrix/federation/v1/version" | json_field server_version || true)

{
	echo "# Интероп-прогон с живым Synapse"
	echo
	echo "- Дата: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "- Synapse: ${SYNAPSE_VERSION:-unknown} (образ matrixdotorg/synapse:v1.165.0)"
	echo "- Наш сервер: a.test, комнаты room_version 10, федерация HTTPS + self-signed CA"
	echo "- Комната: \`$ROOM_ID\`"
	echo "- Вердикт: **$VERDICT**"
	echo
	echo "| Сценарий | Результат |"
	echo "|---|---|"
	for row in "${RESULTS[@]}"; do
		echo "| ${row%|*} | ${row#*|} |"
	done
	for row in "${SKIPPED[@]:-}"; do
		[[ -n $row ]] && echo "| ${row%% — *} | SKIP (${row#*— }) |"
	done
	echo
	echo "Сверка: \`room_state\` нашего сервера (PostgreSQL) против admin API Synapse"
	echo "\`GET /_synapse/admin/v1/rooms/{roomId}/state\`; расхождение — ненулевой код"
	echo "(\`cmd/syngo-interop\`), выгрузка — \`deploy/stand/interop-state.json\`."
} > "$REPORT_MD"

if [[ $KEEP -eq 0 ]]; then
	log "останавливаю interop-стенд"
	compose down >/dev/null
fi

[[ $RESULT -eq 0 ]] || fail "интероп-прогон нашёл расхождения (см. $REPORT_MD)"
echo "PASS: интероп с Synapse $SYNAPSE_VERSION — расхождений нет"
