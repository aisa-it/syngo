#!/usr/bin/env bash
# Сквозная проверка бэкапа и восстановления прод-compose.
# Протокол прогона - docs/operations.md, раздел «Проверка восстановления».
#
#   deploy/prod/backup-restore-compose.sh
#   BRC_PORT=48100 KEEP=1 deploy/prod/backup-restore-compose.sh
#
# Сценарий:
#   1. проект A: сборка образа, up, пользователь, комната, сообщения, медиа;
#   2. backup-compose.sh backup под записью (bob пишет во время бэкапа),
#      с шифрованием gpg (временный ключ), если gpg есть;
#   3. после бэкапа - новые сообщения и файл медиа; verify проходит (сверка с
#      манифестом бэкапа, а не с живой базой), том медиа A не меняется;
#   4. испорченная копия бэкапа - verify падает на контрольных суммах;
#   5. проект B с нуля: секреты из бэкапа, restore, up; вход тем же паролем,
#      история комнаты, sha256 медиа, /sync с токеном проекта A;
#   6. второй syngod на базе B не стартует.
# Все ресурсы с префиксом проекта, удаляются в конце (KEEP=1 - оставить).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
COMPOSE_FILE=$ROOT/deploy/prod/docker-compose.yml
BRC=brc-$$
PORT=${BRC_PORT:-48100}
WORK=$(mktemp -d)
IMAGE=syngo/syngod:$BRC
HELPER_IMAGE=postgres:17-alpine

say() { echo "== $*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
rand() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }
json() { python3 -I -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d[int(k)] if isinstance(d,list) else d.get(k,"")
print(d if not isinstance(d,(dict,list)) else json.dumps(d))' "$1"; }

SHARED_SECRET=$(rand 32)
cat >"$WORK/a.env" <<EOF
SYNGOD_SERVER_NAME=brc.example.org
POSTGRES_PASSWORD=$(rand 24)
SYNGOD_SYNC_TOKEN_SECRET=$(rand 32)
SYNGOD_REGISTRATION_SHARED_SECRET=$SHARED_SECRET
SYNGOD_PORT=$PORT
SYNGOD_OPS_PORT=$((PORT + 1))
SYNGO_IMAGE=$IMAGE
EOF

dca() { docker compose -p "syngo-$BRC-a" --env-file "$WORK/a.env" -f "$COMPOSE_FILE" "$@"; }
dcb() { docker compose -p "syngo-$BRC-b" --env-file "$WORK/secrets/.env" -f "$COMPOSE_FILE" "$@"; }

cleanup() {
	local rc=$?
	if [[ $rc != 0 ]]; then
		echo "--- логи A" >&2
		dca logs --no-color --tail=30 >&2 || true
		[[ -f $WORK/secrets/.env ]] && { echo "--- логи B" >&2; dcb logs --no-color --tail=30 >&2 || true; }
	fi
	if [[ -n ${KEEP:-} ]]; then
		echo "оставлено (KEEP=1): проекты syngo-$BRC-a/b, каталог $WORK"
		return
	fi
	dca down -v --remove-orphans >/dev/null 2>&1 || true
	[[ -f $WORK/secrets/.env ]] && dcb down -v --remove-orphans >/dev/null 2>&1 || true
	docker image rm "$IMAGE" >/dev/null 2>&1 || true
	rm -rf "$WORK"
	exit "$rc"
}
trap cleanup EXIT

# req BASE METHOD PATH [BODY] [TOKEN] -> тело; код ответа - в $WORK/code.
req() {
	local base=$1 method=$2 path=$3 body=${4:-} token=${5:-}
	local args=(-sS -o "$WORK/body" -w '%{http_code}' -X "$method" "$base$path")
	[[ -n $token ]] && args+=(-H "Authorization: Bearer $token")
	[[ -n $body ]] && args+=(-H 'Content-Type: application/json' --data-binary "$body")
	curl "${args[@]}" >"$WORK/code"
	cat "$WORK/body"
}

# send BASE TXN [ROOM TOKEN]: сообщение (лимит rate_limit.send - 10 подряд на пользователя).
send() {
	local room=${3:-$ROOM} token=${4:-$TOKEN}
	req "$1" PUT "/_matrix/client/v3/rooms/$room/send/m.room.message/$2" "{\"msgtype\":\"m.text\",\"body\":\"$2\"}" "$token" >/dev/null
	[[ $(cat "$WORK/code") == 200 ]] || fail "send $2: $(cat "$WORK/code")"
}

# register NAME PASSWORD -> access token (регистрация оператором).
register() {
	local nonce mac
	nonce=$(req "$A" GET /_synapse/admin/v1/register | json nonce)
	mac=$(printf '%s\0%s\0%s\0notadmin' "$nonce" "$1" "$2" | openssl dgst -sha1 -hmac "$SHARED_SECRET" | awk '{print $NF}')
	req "$A" POST /_synapse/admin/v1/register \
		"{\"nonce\":\"$nonce\",\"username\":\"$1\",\"password\":\"$2\",\"mac\":\"$mac\",\"admin\":false}" | json access_token
}

volume_listing() {
	docker run --rm --network none -v "$1:/syngo-media:ro" "$HELPER_IMAGE" \
		sh -c 'cd /syngo-media && find . -type f | sort | xargs -r sha256sum'
}

media_volume() {
	docker volume ls -q --filter "label=com.docker.compose.project=$1" --filter "label=com.docker.compose.volume=media"
}

A=http://127.0.0.1:$PORT

say "1. сборка образа и проект A"
docker build -q -t "$IMAGE" "$ROOT" >/dev/null
dca up -d --wait --wait-timeout 120

PASS=$(rand 12)
TOKEN=$(register alice "$PASS")
[[ -n $TOKEN ]] || fail "регистрация alice: $(cat "$WORK/body")"
ROOM=$(req "$A" POST /_matrix/client/v3/createRoom '{"name":"backup-restore"}' "$TOKEN" | json room_id)
BOB=$(register bob "$(rand 12)")
BOB_ROOM=$(req "$A" POST /_matrix/client/v3/createRoom '{"name":"writer"}' "$BOB" | json room_id)

for i in $(seq 1 7); do send "$A" "before-$i"; done

head -c 50000 /dev/urandom >"$WORK/upload.bin"
MXC=$(curl -fsS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/octet-stream' \
	--data-binary @"$WORK/upload.bin" "$A/_matrix/media/v3/upload?filename=a.bin" | json content_uri)
SINCE=$(req "$A" GET "/_matrix/client/v3/sync?timeout=0" "" "$TOKEN" | json next_batch)
say "A: alice, комната $ROOM, 7 сообщений, медиа $MXC"

say "2. бэкап под записью"
gpg_env=()
if command -v gpg >/dev/null; then
	export GNUPGHOME=$WORK/gnupg
	mkdir -m 700 "$GNUPGHOME"
	gpg --batch --quiet --passphrase '' --quick-gen-key "syngo-backup-$BRC@example.org" default default never 2>/dev/null
	gpg_env=(BACKUP_GPG_RECIPIENT="syngo-backup-$BRC@example.org")
fi

# Пишет bob в своей комнате: у alice остается лимит на сообщения после бэкапа.
(for i in $(seq 1 9); do send "$A" "during-$i" "$BOB_ROOM" "$BOB"; sleep 0.3; done) &
writer=$!
env "${gpg_env[@]}" COMPOSE_PROJECT="syngo-$BRC-a" COMPOSE_ENV_FILE="$WORK/a.env" \
	BACKUP_DIR="$WORK/backups" BACKUP_METRICS_FILE="$WORK/metrics/syngo_backup.prom" \
	"$ROOT/deploy/prod/backup-compose.sh" backup
wait "$writer"

BACKUP=$(find "$WORK/backups" -mindepth 1 -maxdepth 1 -name 'syngo-*' | sort | tail -n1)
[[ -n $BACKUP ]] || fail "бэкап не создан"
perms=$(find "$WORK/backups" -mindepth 1 \( -type f -o -type d \) ! -name .lock -printf '%m\n' | sort -u | tr '\n' ' ')
[[ $perms == "600 " || $perms == "600 700 " ]] || fail "права в бэкапе: $perms"
grep -q '^syngo_backup_last_success_timestamp_seconds [1-9]' "$WORK/metrics/syngo_backup.prom" || fail "нет метрики успешного бэкапа"
say "бэкап: $(basename "$BACKUP"), права $perms"

say "3. после бэкапа: новые сообщения и медиа; verify не трогает живые данные"
for i in 1 2; do send "$A" "after-$i"; done
head -c 1000 /dev/urandom | curl -fsS -X POST -H "Authorization: Bearer $TOKEN" \
	-H 'Content-Type: application/octet-stream' --data-binary @- "$A/_matrix/media/v3/upload?filename=b.bin" >/dev/null
vol_a=$(media_volume "syngo-$BRC-a")
volume_listing "$vol_a" >"$WORK/vol-before"
COMPOSE_PROJECT="syngo-$BRC-a" COMPOSE_ENV_FILE="$WORK/a.env" "$ROOT/deploy/prod/backup-compose.sh" verify "$BACKUP"
volume_listing "$vol_a" | diff -u "$WORK/vol-before" - || fail "verify изменил том медиа A"

say "4. испорченная копия бэкапа"
broken=$WORK/broken
mkdir -m 700 "$broken"

if [[ $BACKUP == *.tar.gpg ]]; then
	gpg --batch --decrypt "$BACKUP" 2>/dev/null | tar -xf - -C "$broken"
else
	cp -a "$BACKUP" "$broken/"
fi

victim=$(find "$broken" -name media.tar.gz)
printf 'x' | dd of="$victim" bs=1 seek=100 conv=notrunc status=none
if COMPOSE_PROJECT="syngo-$BRC-a" COMPOSE_ENV_FILE="$WORK/a.env" \
	"$ROOT/deploy/prod/backup-compose.sh" verify "$(dirname "$victim")" >"$WORK/broken.log" 2>&1; then
	fail "испорченный бэкап прошел verify"
fi
grep -q "контрольные суммы не сошлись" "$WORK/broken.log" || fail "испорченный бэкап: $(tail -3 "$WORK/broken.log")"

say "5. восстановление в чистый проект B"
# Секреты нужны до запуска postgres B: берем .env из бэкапа (в испорченной
# копии он цел - испорчен только архив медиа).
mkdir -m 700 "$WORK/secrets"
cp "$(dirname "$victim")/secrets/a.env" "$WORK/secrets/.env"

# Секреты те же (сервер, токены /sync), порты и образ - свои у B.
sed -i -e "s/^SYNGOD_PORT=.*/SYNGOD_PORT=$((PORT + 2))/" -e "s/^SYNGOD_OPS_PORT=.*/SYNGOD_OPS_PORT=$((PORT + 3))/" "$WORK/secrets/.env"
dcb create >/dev/null 2>&1
dcb up -d --wait --wait-timeout 60 postgres
COMPOSE_PROJECT="syngo-$BRC-b" COMPOSE_ENV_FILE="$WORK/secrets/.env" "$ROOT/deploy/prod/backup-compose.sh" restore "$BACKUP"
dcb up -d --wait --wait-timeout 120
B=http://127.0.0.1:$((PORT + 2))

login=$(req "$B" POST /_matrix/client/v3/login \
	"{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"alice\"},\"password\":\"$PASS\"}")
[[ $(cat "$WORK/code") == 200 ]] || fail "B: вход alice: $login"
TOKEN_B=$(json access_token <<<"$login")

messages=$(req "$B" GET "/_matrix/client/v3/rooms/$ROOM/messages?dir=b&limit=200" "" "$TOKEN_B" |
	python3 -I -c 'import json,sys; print(sum(1 for e in json.load(sys.stdin)["chunk"] if e["type"]=="m.room.message"))')
((messages == 7)) || fail "B: в комнате $messages сообщений, ожидали 7 (до бэкапа)"
grep -q '"before-7"' "$WORK/body" || fail "B: нет сообщения before-7"
if grep -q '"after-1"' "$WORK/body"; then fail "B: сообщение после бэкапа попало в восстановление"; fi

curl -fsS -H "Authorization: Bearer $TOKEN_B" "$B/_matrix/client/v1/media/download/${MXC#mxc://}" -o "$WORK/download.bin"
cmp -s "$WORK/upload.bin" "$WORK/download.bin" || fail "B: медиа отличается от загруженного в A"

req "$B" GET "/_matrix/client/v3/sync?timeout=0&since=$SINCE" "" "$TOKEN_B" >/dev/null
[[ $(cat "$WORK/code") == 200 ]] || fail "B: /sync с токеном A: $(cat "$WORK/code")"
say "B: вход, $messages сообщений до бэкапа (после - нет), медиа совпадает, /sync с токеном A"

say "6. второй syngod на базе B"
if dcb run --rm --no-deps -T syngod >"$WORK/second.log" 2>&1; then
	fail "второй экземпляр стартовал"
fi
grep -q "already used by another syngod instance" "$WORK/second.log" || fail "второй экземпляр: $(tail -3 "$WORK/second.log")"
grep -o 'application_name=[^)]*' "$WORK/second.log" | head -n1

say "OK: бэкап -> verify -> restore в чистый проект проходит; второй экземпляр отклонен"
