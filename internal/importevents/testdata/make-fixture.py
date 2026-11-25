#!/usr/bin/env python3
"""Сценарий для фикстуры живого Synapse.

Наполняет запущенный Synapse через Client-Server API: комнаты v10, v1 (ссылки
prev/auth парами) и v11, сообщения, смена темы, redaction своего и чужого
сообщения, \\u0000 в content, выход и вход участника, алиас.

    make-fixture.py http://127.0.0.1:38008 c4.test

Пользователи alice (admin), bob, carol с паролем pass-<имя> должны быть созданы
заранее (register_new_matrix_user). Порядок получения фикстуры - в README.md рядом.
"""
import json
import sys
import urllib.parse
import urllib.request

BASE = sys.argv[1]
SERVER = sys.argv[2]
txn = 0


def call(method, path, token=None, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    with urllib.request.urlopen(req) as resp:
        return json.loads(resp.read() or b"{}")


def login(user):
    return call("POST", "/_matrix/client/v3/login", body={
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": user},
        "password": "pass-" + user,
    })["access_token"]


def room_path(room_id):
    return "/_matrix/client/v3/rooms/" + urllib.parse.quote(room_id)


def send(token, room_id, body, etype="m.room.message"):
    global txn
    txn += 1
    content = body if isinstance(body, dict) else {"msgtype": "m.text", "body": body}
    return call("PUT", room_path(room_id) + "/send/" + etype + "/t" + str(txn), token, content)["event_id"]


def state(token, room_id, etype, content, key=""):
    return call("PUT", room_path(room_id) + "/state/" + etype + "/" + key, token, content)["event_id"]


def redact(token, room_id, event_id, reason):
    global txn
    txn += 1
    return call("PUT", room_path(room_id) + "/redact/" + urllib.parse.quote(event_id) + "/r" + str(txn),
                token, {"reason": reason})["event_id"]


def join(token, room_id):
    return call("POST", "/_matrix/client/v3/join/" + urllib.parse.quote(room_id), token, {})


def leave(token, room_id):
    return call("POST", room_path(room_id) + "/leave", token, {})


alice, bob, carol = login("alice"), login("bob"), login("carol")
marks = {}

# Комната v10: основной сценарий.
r10 = call("POST", "/_matrix/client/v3/createRoom", alice, {
    "room_version": "10", "preset": "public_chat", "name": "Общая", "topic": "тема 1",
    "room_alias_name": "general",
})["room_id"]
join(bob, r10)
join(carol, r10)
send(alice, r10, "привет всем")
marks["v10_bob_own_redacted"] = send(bob, r10, "секрет боба")
send(carol, r10, "сообщение кэрол")
marks["v10_topic_old"] = state(alice, r10, "m.room.topic", {"topic": "тема 2"})
marks["v10_nul"] = send(alice, r10, "с нулем \u0000 внутри")
marks["v10_bob_by_admin"] = send(bob, r10, "это удалит админ")
marks["v10_rejected"] = send(carol, r10, "станет отклоненным")
marks["v10_soft_failed"] = send(carol, r10, "станет soft-failed")
marks["v10_outlier"] = send(bob, r10, "станет outlier")
marks["v10_backfill"] = send(alice, r10, "станет backfill (stream < 0)")
marks["v10_redaction_own"] = redact(bob, r10, marks["v10_bob_own_redacted"], "сам")
marks["v10_redaction_admin"] = redact(alice, r10, marks["v10_bob_by_admin"], "модерация")
state(alice, r10, "m.room.topic", {"topic": "тема 3"})
send(bob, r10, {"msgtype": "m.text", "body": "ответ",
                "m.relates_to": {"m.in_reply_to": {"event_id": marks["v10_nul"]}}})
leave(carol, r10)
send(alice, r10, "после выхода кэрол")

# Комната v1: prev_events/auth_events парами [id, {sha256}], event_id не хеш.
r1 = call("POST", "/_matrix/client/v3/createRoom", alice, {
    "room_version": "1", "preset": "public_chat", "name": "Старая v1",
})["room_id"]
join(bob, r1)
send(alice, r1, "v1 первое")
marks["v1_redacted"] = send(bob, r1, "v1 удалю")
marks["v1_redaction"] = redact(bob, r1, marks["v1_redacted"], "v1")
send(bob, r1, "v1 последнее")

# Комната v11: redacts в content.
r11 = call("POST", "/_matrix/client/v3/createRoom", alice, {
    "room_version": "11", "preset": "private_chat", "name": "Новая v11", "invite": ["@bob:" + SERVER],
})["room_id"]
join(bob, r11)
marks["v11_redacted"] = send(alice, r11, "v11 удалю")
marks["v11_redaction"] = redact(alice, r11, marks["v11_redacted"], "v11")
send(bob, r11, "v11 последнее")

marks.update({"room_v10": r10, "room_v1": r1, "room_v11": r11})
print(json.dumps(marks, ensure_ascii=False, indent=2))
