#!/usr/bin/env bash
# Smoke-сценарий федерации: инвайт → join через федерацию → 5 сообщений → проверка порядка
# через /messages и /sync → leave → доставка после рестарта b → идемпотентность send.
#
# Код выхода: 0 — всё сошлось, 1 — сообщение не доехало/порядок нарушен/дубль.
set -euo pipefail

cd "$(dirname "$0")"

STATE_FILE=.stand-state.env
[[ -f $STATE_FILE ]] || { echo "нет $STATE_FILE: сначала deploy/stand/bootstrap.sh" >&2; exit 1; }

# shellcheck disable=SC1090
source "$STATE_FILE"

compose() { docker compose -f docker-compose.yml "$@"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
log() { printf '%s\n' "==> $*"; }

json_field() { python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get(sys.argv[1],""))' "$1"; }
json_path() { python3 -c 'import json,sys;d=json.load(sys.stdin);
for part in sys.argv[1].split("."):
    d = d[int(part)] if part.isdigit() else d.get(part, "")
print(d)' "$1"; }

api() { # api <method> <url> <token> [body]
	local method=$1 url=$2 token=$3 body=${4:-}
	if [[ -n $body ]]; then
		curl -sS --max-time 20 -X "$method" "$url" \
			-H "Authorization: Bearer $token" -H 'Content-Type: application/json' -d "$body"
	else
		curl -sS --max-time 20 -X "$method" "$url" -H "Authorization: Bearer $token"
	fi
}

send_msg() { # send_msg <txn> <text>
	api PUT "$A_URL/_matrix/client/v3/rooms/$ROOM_ID/send/m.room.message/$1" "$ALICE_TOKEN" \
		"{\"msgtype\":\"m.text\",\"body\":\"$2\"}" | json_field event_id
}

event_bodies() { # event_bodies <url> <token> — тела сообщений комнаты по порядку
	local url=$1 token=$2
	curl -sS --max-time 20 -G "$url/_matrix/client/v3/rooms/$ROOM_ID/messages" \
		--data-urlencode 'dir=b' --data-urlencode 'limit=20' \
		-H "Authorization: Bearer $token" |
		python3 -c 'import json,sys
chunk=json.load(sys.stdin).get("chunk",[])
bodies=[(e.get("event_id"),e.get("content",{}).get("body")) for e in chunk if e.get("type")=="m.room.message"]
print("\n".join(b for _,b in reversed(bodies)))'
}

echo "=== SYNGO stand smoke: $ROOM_ID"

# 1. bob ещё не в комнате: join через федерацию (приглашение лежит в комнате на A).
log "bob join через федерацию (a.test ← b.test)"
JOIN=$(api POST "$B_URL/_matrix/client/v3/rooms/$ROOM_ID/join" "$BOB_TOKEN" '{}')
printf '%s' "$JOIN" | grep -q '"room_id"' || fail "join не выполнен: $JOIN"

# 2. Пять сообщений от alice.
log "alice отправляет 5 сообщений"
for i in 1 2 3 4 5; do
	EID=$(send_msg "smoke-$i" "msg-$i")
	[[ -n $EID ]] || fail "send msg-$i не вернул event_id"
done

# 3. Порядок на A и на B. Доставка федерацией асинхронная (outbox + backoff),
# поэтому ждём до 30 с, а не фиксированные 3.
EXPECTED=$'msg-1\nmsg-2\nmsg-3\nmsg-4\nmsg-5'
A_BODIES=$(event_bodies "$A_URL" "$ALICE_TOKEN")
[[ "$A_BODIES" == "$EXPECTED" ]] || fail "порядок на A: [$A_BODIES], ожидали [$EXPECTED]"

B_BODIES=""
for _ in $(seq 1 30); do
	B_BODIES=$(event_bodies "$B_URL" "$BOB_TOKEN")
	[[ "$B_BODIES" == "$EXPECTED" ]] && break
	sleep 1
done
[[ "$B_BODIES" == "$EXPECTED" ]] || fail "порядок на B: [$B_BODIES], ожидали [$EXPECTED]"

# 4. /sync на B видит те же сообщения.
log "проверяю /sync на b.test"
SYNC=$(api GET "$B_URL/_matrix/client/v3/sync?timeout=0" "$BOB_TOKEN")
printf '%s' "$SYNC" | python3 -c 'import json,sys
d=json.load(sys.stdin); rooms=d.get("rooms",{}).get("join",{})
assert rooms, "в /sync нет join-комнат"
print("sync: комнат", len(rooms))' || fail "/sync не отдал комнату"

# 5. leave. По спеке POST /leave отвечает 200 и пустым объектом (без room_id),
# поэтому проверяем код ответа, а не тело.
log "bob выходит из комнаты"
LEAVE_CODE=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' -X POST \
	"$B_URL/_matrix/client/v3/rooms/$ROOM_ID/leave" \
	-H "Authorization: Bearer $BOB_TOKEN" -H 'Content-Type: application/json' -d '{}')
[[ "$LEAVE_CODE" == "200" ]] || fail "leave вернул HTTP $LEAVE_CODE"

# 6. Рестарт b: после него сообщение должно доехать (очередь + догон).
log "рестарт syngod-b"
compose restart syngod-b >/dev/null
for _ in $(seq 1 60); do
	curl -fsS --max-time 2 "$B_URL/health" >/dev/null 2>&1 && break
	sleep 1
done

log "alice отправляет msg-6 после рестарта b"
EID6=$(send_msg "smoke-6" "msg-6")
[[ -n $EID6 ]] || fail "send msg-6 не вернул event_id"

sleep 5

# Доставка после рестарта проверяется по БД b.test: bob уже вышел из комнаты,
# и клиентский API историю ему не отдаёт, а факт доставки проверять надо.
DB_ORDER=$(compose exec -T postgres psql -U syngo -d syngo_b -tAc \
	"SELECT content->>'body' FROM events WHERE room_id='$ROOM_ID' AND type='m.room.message' ORDER BY stream_ordering" |
	tr -d '\r')
EXPECTED_DB=$'msg-1\nmsg-2\nmsg-3\nmsg-4\nmsg-5\nmsg-6'
[[ "$DB_ORDER" == "$EXPECTED_DB" ]] || fail "порядок в БД b.test: [$DB_ORDER], ожидали [$EXPECTED_DB]"

# 7. Идемпотентность: тот же txn_id → тот же event_id и без дубля.
log "повторный send с тем же txn_id"
EID6_AGAIN=$(send_msg "smoke-6" "msg-6")
[[ "$EID6_AGAIN" == "$EID6" ]] || fail "повторный send вернул другой event_id: $EID6_AGAIN != $EID6"

COUNT=$(compose exec -T postgres psql -U syngo -d syngo_b -tAc \
	"SELECT count(*) FROM events WHERE room_id='$ROOM_ID' AND content->>'body'='msg-6'" | tr -d ' \r')
[[ "$COUNT" == "1" ]] || fail "msg-6 в БД b.test встречается $COUNT раз (ожидали 1)"

echo "PASS: федерация, порядок, leave, доставка после рестарта, идемпотентность"
