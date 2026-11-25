#!/usr/bin/env bash
# Smoke после деплоя: health/ready, versions, регистрация и вход
# по настройкам сервера, комната, сообщение, /sync, upload и download медиа.
#
#   BASE=http://127.0.0.1:8008 SMOKE_SHARED_SECRET=... deploy/prod/smoke.sh
#
# Как получить пользователя (по настройкам):
#   SMOKE_SHARED_SECRET - регистрация оператором через /_synapse/admin/v1/register
#                         (значение SYNGOD_REGISTRATION_SHARED_SECRET);
#   SMOKE_REGISTRATION=open - открытая регистрация /register (UIA m.login.dummy);
#   иначе - вход существующим SMOKE_USER/SMOKE_PASSWORD.
# SMOKE_EXPECT_REGISTRATION=closed|open|any - что ожидать от /register без секрета
# (по умолчанию closed: прод закрыт).
# Созданный smoke-пользователь в конце деактивируется.
set -euo pipefail

BASE=${BASE:-http://127.0.0.1:8008}
EXPECT_REG=${SMOKE_EXPECT_REGISTRATION:-closed}
SUFFIX=$(date +%s)-$RANDOM
USER_NAME=${SMOKE_USER:-smoke-$SUFFIX}
PASS=${SMOKE_PASSWORD:-$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')}
CREATED=0

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
json() { python3 -c 'import json,sys
d=json.load(sys.stdin)
for k in sys.argv[1].split("."):
    d=d.get(k,"") if isinstance(d,dict) else ""
print(d if not isinstance(d,(dict,list)) else json.dumps(d))' "$1"; }

# req METHOD PATH [BODY] [TOKEN] -> печатает "<code> <body>".
req() {
	local method=$1 path=$2 body=${3:-} token=${4:-}
	local args=(-sS -o /tmp/smoke.$$ -w '%{http_code}' -X "$method" "$BASE$path")
	[[ -n $token ]] && args+=(-H "Authorization: Bearer $token")
	[[ -n $body ]] && args+=(-H 'Content-Type: application/json' --data-binary "$body")
	local code
	code=$(curl "${args[@]}") || fail "$method $path: curl"
	echo "$code $(cat /tmp/smoke.$$)"
	rm -f /tmp/smoke.$$
}

expect() { # expect CODE "<code> <body>" WHAT
	[[ ${2%% *} == "$1" ]] || fail "$3: ждали $1, получили ${2:0:300}"
}

cleanup() {
	if [[ $CREATED == 1 && -n ${TOKEN:-} ]]; then
		r=$(req POST /_matrix/client/v3/account/deactivate \
			"{\"auth\":{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"$USER_NAME\"},\"password\":\"$PASS\"}}" "$TOKEN" || true)
		[[ ${r%% *} == 200 ]] && echo "ok: smoke-пользователь $USER_NAME деактивирован" ||
			echo "warn: не удалось деактивировать $USER_NAME: ${r:0:200}" >&2
	fi
}
trap cleanup EXIT

r=$(req GET /health); expect 200 "$r" "/health"; ok "/health"
r=$(req GET /health/ready); expect 200 "$r" "/health/ready"; ok "/health/ready (БД и схема)"
r=$(req GET /_matrix/client/versions); expect 200 "$r" "/versions"
[[ $r == *'"v1.'* ]] || fail "/versions без версий спецификации: $r"; ok "/_matrix/client/versions"

# Открытая регистрация: проверяем, что настройки сервера совпадают с ожидаемыми.
reg_body="{\"username\":\"probe-$SUFFIX\",\"password\":\"$PASS\"}"
r=$(req POST /_matrix/client/v3/register "$reg_body")
case "$EXPECT_REG" in
	closed) expect 403 "$r" "/register при закрытой регистрации"; ok "/register закрыт (403)" ;;
	open) expect 401 "$r" "/register при открытой регистрации (UIA)"; ok "/register открыт (UIA)" ;;
	any) ok "/register: ${r%% *}" ;;
	*) fail "SMOKE_EXPECT_REGISTRATION=$EXPECT_REG" ;;
esac

if [[ -n ${SMOKE_SHARED_SECRET:-} ]]; then
	r=$(req GET /_synapse/admin/v1/register); expect 200 "$r" "nonce"
	NONCE=$(json nonce <<<"${r#* }")
	MAC=$(SECRET=$SMOKE_SHARED_SECRET python3 -c 'import hmac,hashlib,os,sys
msg=b"\0".join(a.encode() for a in sys.argv[1:4]+["notadmin"])
print(hmac.new(os.environ["SECRET"].encode(),msg,hashlib.sha1).hexdigest())' "$NONCE" "$USER_NAME" "$PASS")
	r=$(req POST /_synapse/admin/v1/register \
		"{\"nonce\":\"$NONCE\",\"username\":\"$USER_NAME\",\"password\":\"$PASS\",\"admin\":false,\"mac\":\"$MAC\"}")
	expect 200 "$r" "регистрация через общий секрет"
	CREATED=1; ok "регистрация оператором: @$USER_NAME"
elif [[ ${SMOKE_REGISTRATION:-} == open ]]; then
	body="{\"username\":\"$USER_NAME\",\"password\":\"$PASS\",\"device_id\":\"SMOKE\"}"
	r=$(req POST /_matrix/client/v3/register "$body"); expect 401 "$r" "UIA шаг 1"
	SESSION=$(json session <<<"${r#* }")
	[[ -n $SESSION ]] || fail "UIA без session: $r"
	r=$(req POST /_matrix/client/v3/register \
		"{\"username\":\"$USER_NAME\",\"password\":\"$PASS\",\"device_id\":\"SMOKE\",\"auth\":{\"type\":\"m.login.dummy\",\"session\":\"$SESSION\"}}")
	expect 200 "$r" "UIA шаг 2"
	CREATED=1; ok "открытая регистрация: @$USER_NAME"
else
	[[ -n ${SMOKE_USER:-} && -n ${SMOKE_PASSWORD:-} ]] ||
		fail "нужен SMOKE_SHARED_SECRET, SMOKE_REGISTRATION=open или SMOKE_USER/SMOKE_PASSWORD"
fi

r=$(req POST /_matrix/client/v3/login \
	"{\"type\":\"m.login.password\",\"identifier\":{\"type\":\"m.id.user\",\"user\":\"$USER_NAME\"},\"password\":\"$PASS\",\"device_id\":\"SMOKE\"}")
expect 200 "$r" "login"
TOKEN=$(json access_token <<<"${r#* }")
[[ -n $TOKEN ]] || fail "login без access_token"; ok "вход паролем"

r=$(req GET /_matrix/client/v3/account/whoami "" "$TOKEN"); expect 200 "$r" "whoami"

r=$(req POST /_matrix/client/v3/createRoom '{"name":"deploy-smoke","preset":"private_chat"}' "$TOKEN")
expect 200 "$r" "createRoom"
ROOM=$(json room_id <<<"${r#* }"); ok "комната $ROOM"

r=$(req PUT "/_matrix/client/v3/rooms/$ROOM/send/m.room.message/smoke-$SUFFIX" \
	'{"msgtype":"m.text","body":"deploy smoke"}' "$TOKEN")
expect 200 "$r" "send"; ok "сообщение"

r=$(req GET "/_matrix/client/v3/sync?timeout=0" "" "$TOKEN"); expect 200 "$r" "/sync"
[[ $r == *"$ROOM"* ]] || fail "/sync без созданной комнаты"; ok "/sync"

# Медиа: upload в том и download обратно (том media принадлежит nonroot).
PAYLOAD="syngo smoke $SUFFIX"
code=$(curl -sS -o /tmp/smoke-up.$$ -w '%{http_code}' -X POST "$BASE/_matrix/media/v3/upload?filename=smoke.txt" \
	-H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/plain' --data-binary "$PAYLOAD")
up=$(cat /tmp/smoke-up.$$); rm -f /tmp/smoke-up.$$
[[ $code == 200 ]] || fail "upload: $code $up"
MXC=$(json content_uri <<<"$up")
MEDIA=${MXC#mxc://}
got=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$BASE/_matrix/client/v1/media/download/$MEDIA") || fail "download $MXC"
[[ $got == "$PAYLOAD" ]] || fail "download вернул другое содержимое"; ok "медиа upload/download ($MXC)"

echo "OK: деплой работает ($BASE)"
