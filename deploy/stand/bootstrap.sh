#!/usr/bin/env bash
# Подготовка локального стенда федерации SYNGO: .env, ключи, БД, пользователи, комната.
#
# Идемпотентен: повторный запуск не пересоздаёт ключи и не падает на существующих комнатах.
# Требования: docker + docker compose v2, python3 (разбор JSON), curl.
set -euo pipefail

cd "$(dirname "$0")"

A_URL=${A_URL:-http://127.0.0.1:18008}
B_URL=${B_URL:-http://127.0.0.1:18009}
ALICE_PASSWORD=${ALICE_PASSWORD:-stand-alice-password}
BOB_PASSWORD=${BOB_PASSWORD:-stand-bob-password}
STATE_FILE=.stand-state.env

log() { printf '%s\n' "==> $*"; }
fail() { printf 'ОШИБКА: %s\n' "$*" >&2; exit 1; }

command -v docker >/dev/null || fail "нужен docker"
docker compose version >/dev/null || fail "нужен docker compose v2"
command -v curl >/dev/null || fail "нужен curl"
command -v python3 >/dev/null || fail "нужен python3"

compose() { docker compose -f docker-compose.yml "$@"; }

# JSON-поле из stdin: json_field '.room_id'
json_field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1],""))' "$1"; }

wait_health() {
	local url=$1 name=$2 i
	for i in $(seq 1 60); do
		if curl -fsS --max-time 2 "$url/health" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	fail "$name не поднялся за 60 секунд ($url/health)"
}

register_user() {
	local url=$1 user=$2 pass=$3
	local body
	body=$(curl -sS --max-time 10 -X POST "$url/_matrix/client/v3/register" \
		-H 'Content-Type: application/json' \
		-d "{\"username\":\"$user\",\"password\":\"$pass\",\"device_id\":\"STAND\"}")

	# Регистрация всегда отвечает 401 с UIA-флоу (m.login.dummy): проходим шаг
	# с полученным session и повторяем запрос.
	if printf '%s' "$body" | grep -q '"session"'; then
		local session
		session=$(printf '%s' "$body" | json_field session)
		body=$(curl -sS --max-time 10 -X POST "$url/_matrix/client/v3/register" \
			-H 'Content-Type: application/json' \
			-d "{\"username\":\"$user\",\"password\":\"$pass\",\"device_id\":\"STAND\",\"auth\":{\"type\":\"m.login.dummy\",\"session\":\"$session\"}}")
	fi

	if printf '%s' "$body" | grep -q '"access_token"'; then
		printf '%s' "$body" | json_field access_token
		return 0
	fi

	# Пользователь уже есть — логинимся.
	body=$(curl -sS --max-time 10 -X POST "$url/_matrix/client/v3/login" \
		-H 'Content-Type: application/json' \
		-d "{\"type\":\"m.login.password\",\"user\":\"$user\",\"password\":\"$pass\",\"device_id\":\"STAND\"}")
	printf '%s' "$body" | json_field access_token
}

# 1. Секреты.
if [[ ! -f .env ]]; then
	log "создаю .env из .env.example"
	cp .env.example .env
	chmod 600 .env
fi

# .env читает docker compose, но не shell: без этого DSN собирается с паролем по умолчанию,
# а postgres поднимается с паролем из .env — миграции падают на SASL auth.
set -a
# shellcheck disable=SC1091
. ./.env
set +a

# 2. Ключи подписи (в git не попадают).
mkdir -p keys
for k in a b; do
	# Docker создаёт отсутствующий bind-mount как КАТАЛОГ, поэтому сначала файл-заглушка.
	if [[ -d keys/$k.key ]]; then
		rm -rf "keys/$k.key"
	fi

	if [[ ! -f keys/$k.key ]]; then
		log "генерирую ключ сервера $k"
		if command -v go >/dev/null 2>&1; then
			# bootstrap работает из deploy/stand — go run запускаем из корня репозитория.
			(cd ../.. && go run ./cmd/federation-keygen -version a_1 -out "deploy/stand/keys/$k.key") >/dev/null
		else
			touch "keys/$k.key"
			compose build syngod-$k >/dev/null
			compose run --rm --no-deps -T \
				-v "$PWD/keys:/keys" --entrypoint /usr/local/bin/syngo-keygen syngod-$k \
				-version a_1 -out "/keys/$k.key" >/dev/null
		fi
		chmod 600 "keys/$k.key"
	fi
done

# Права на ключи: в контейнере стенда syngod работает под uid 10001, поэтому 600
# от хостового пользователя даёт «permission denied». Применяем всегда (идемпотентно),
# а не только при генерации — ключи могли остаться от прошлого запуска.
for k in a b; do
	if sudo -n true 2>/dev/null; then
		sudo -n chown 10001:10001 "keys/$k.key"
	else
		chmod 644 "keys/$k.key"
	fi
done

# 3. PostgreSQL и миграции обеих БД.
log "поднимаю postgres"
compose up -d postgres
for i in $(seq 1 60); do
	if compose exec -T postgres pg_isready -U syngo -d postgres >/dev/null 2>&1; then break; fi
	sleep 1
done

for k in a b; do
	log "миграции БД syngo_$k"
	compose run --rm --no-deps -T --entrypoint /usr/local/bin/syngo-migrate syngod-$k \
		-dsn "postgres://syngo:${POSTGRES_PASSWORD:-syngo}@postgres:5432/syngo_$k?sslmode=disable" \
		-path /opt/syngo/migrations -up
done

# 4. Серверы.
log "запускаю syngod-a и syngod-b"
compose up -d syngod-a syngod-b
wait_health "$A_URL" syngod-a
wait_health "$B_URL" syngod-b

# 5. Пользователи.
log "регистрирую alice@a.test и bob@b.test"
ALICE_TOKEN=$(register_user "$A_URL" alice "$ALICE_PASSWORD")
[[ -n "$ALICE_TOKEN" ]] || fail "не удалось зарегистрировать alice@a.test"
BOB_TOKEN=$(register_user "$B_URL" bob "$BOB_PASSWORD")
[[ -n "$BOB_TOKEN" ]] || fail "не удалось зарегистрировать bob@b.test"

# 6. Комната, приглашение и алиас.
ROOM_ID=""
if [[ -f $STATE_FILE ]]; then
	# shellcheck disable=SC1090
	source "$STATE_FILE"
fi
ROOM_ID=${ROOM_ID:-}

if [[ -z "$ROOM_ID" ]]; then
	log "создаю комнату и приглашаю bob@b.test"
	body=$(curl -sS --max-time 15 -X POST "$A_URL/_matrix/client/v3/createRoom" \
		-H "Authorization: Bearer $ALICE_TOKEN" -H 'Content-Type: application/json' \
		-d '{"name":"stand","preset":"private_chat","invite":["@bob:b.test"]}')
	ROOM_ID=$(printf '%s' "$body" | json_field room_id)
	[[ -n "$ROOM_ID" ]] || fail "createRoom: $body"
else
	log "комната уже есть: $ROOM_ID"
fi

umask 077
cat > "$STATE_FILE" <<STATE
# Состояние стенда: токены и комната. Не коммитится.
A_URL=$A_URL
B_URL=$B_URL
ALICE_TOKEN=$ALICE_TOKEN
BOB_TOKEN=$BOB_TOKEN
ROOM_ID=$ROOM_ID
STATE
chmod 600 "$STATE_FILE"

log "стенд готов: A=$A_URL B=$B_URL комната=$ROOM_ID"
log "дальше: deploy/stand/smoke.sh (или make stand-smoke)"
