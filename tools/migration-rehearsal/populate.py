#!/usr/bin/env python3
"""Наполнение Synapse для репетиции миграции.

    populate.py <base_url> <server_name> <preview_url> [scale]

Через Client-Server и admin API Synapse создает: пользователей (alice - admin,
создается заранее register_new_matrix_user с паролем pass-alice), комнаты v10,
v11 и v1, сообщения с ответами, правками и реакциями, смену темы, алиасы,
приглашение и выход, redaction своих и чужих сообщений, медиа (PNG и бинарные,
одно крупное), карантин двух медиа, превью ссылки (url_cache), деактивацию
пользователя. scale (по умолчанию 1) умножает число сообщений.

Печатает JSON с отметками (комнаты, медиа, карантин) - их проверяет приемка.
"""
import hashlib
import json
import os
import struct
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import zlib

BASE, SERVER, PREVIEW_URL = sys.argv[1], sys.argv[2], sys.argv[3]
SCALE = int(sys.argv[4]) if len(sys.argv) > 4 else 1
TXN = [0]


def call(method, path, token=None, body=None, raw=None, ctype="application/json"):
    data = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", ctype)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=120) as resp:
                return json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as err:
            if err.code == 429 and attempt < 4:
                time.sleep(1)
                continue
            raise RuntimeError(f"{method} {path}: {err.code} {err.read()[:300]!r}") from err
    raise RuntimeError("unreachable")


def login(user, password):
    return call("POST", "/_matrix/client/v3/login", body={
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": user},
        "password": password,
    })["access_token"]


def room_path(room_id):
    return "/_matrix/client/v3/rooms/" + urllib.parse.quote(room_id)


def txn():
    TXN[0] += 1
    return "t" + str(TXN[0])


def send(token, room_id, content, etype="m.room.message"):
    if isinstance(content, str):
        content = {"msgtype": "m.text", "body": content}
    return call("PUT", room_path(room_id) + "/send/" + etype + "/" + txn(), token, content)["event_id"]


def state(token, room_id, etype, content, key=""):
    return call("PUT", room_path(room_id) + "/state/" + etype + "/" + urllib.parse.quote(key), token, content)["event_id"]


def redact(token, room_id, event_id, reason):
    return call("PUT", room_path(room_id) + "/redact/" + urllib.parse.quote(event_id) + "/" + txn(),
                token, {"reason": reason})["event_id"]


def join(token, room_id):
    return call("POST", "/_matrix/client/v3/join/" + urllib.parse.quote(room_id), token, {})


def png(width, height, seed):
    """Несжимаемый PNG: синтетическая картинка с шумом по seed."""
    rows = []
    state_ = seed or 1
    for _ in range(height):
        row = bytearray([0])
        for _ in range(width):
            state_ = (state_ * 1103515245 + 12345) & 0x7FFFFFFF
            row += bytes([state_ & 0xFF, (state_ >> 8) & 0xFF, (state_ >> 16) & 0xFF])
        rows.append(bytes(row))

    def chunk(kind, payload):
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(b"".join(rows))) + chunk(b"IEND", b"")


def upload(token, data, ctype, name):
    path = "/_matrix/media/v3/upload?filename=" + urllib.parse.quote(name)
    return call("POST", path, token, raw=data, ctype=ctype)["content_uri"]


def main():
    alice = login("alice", "pass-alice")
    marks = {"users": ["@alice:" + SERVER], "media": {}, "quarantined": [], "rooms": {}}

    # Пользователи - admin API (быстрее register_new_matrix_user на каждого).
    names = ["bob", "carol", "dave", "erin", "frank", "grace", "heidi", "ivan", "judy",
             "mallory", "niaj", "olivia", "peggy", "rupert", "sybil", "trent", "victor", "walter", "zoe"]
    for name in names:
        call("PUT", "/_synapse/admin/v2/users/" + urllib.parse.quote("@" + name + ":" + SERVER), alice,
             {"password": "pass-" + name, "displayname": name.capitalize()})
        marks["users"].append("@" + name + ":" + SERVER)
    tokens = {name: login(name, "pass-" + name) for name in names}
    tokens["alice"] = alice
    people = ["alice"] + names

    # Комнаты: две v10 (публичная с алиасом и приватная), v11, v1.
    specs = [
        ("general", "10", "public_chat", "Общая", people),
        ("dev", "10", "private_chat", "Разработка", people[:8]),
        ("v11", "11", "private_chat", "Новая v11", people[:5]),
        ("old", "1", "public_chat", "Старая v1", people[:4]),
    ]
    events_sent = 0
    for alias, version, preset, title, members in specs:
        body = {"room_version": version, "preset": preset, "name": title, "topic": title + ": тема 1"}
        if preset == "public_chat":
            body["room_alias_name"] = alias
        room = call("POST", "/_matrix/client/v3/createRoom", alice, body)["room_id"]
        marks["rooms"][alias] = room
        for name in members[1:]:
            if preset == "private_chat":
                call("POST", room_path(room) + "/invite", alice, {"user_id": "@" + name + ":" + SERVER})
            join(tokens[name], room)

        last = None
        count = 120 * SCALE if alias in ("general", "dev") else 30 * SCALE
        for i in range(count):
            who = members[i % len(members)]
            if i % 17 == 5 and last:
                content = {"msgtype": "m.text", "body": f"ответ {i}",
                           "m.relates_to": {"m.in_reply_to": {"event_id": last}}}
            elif i % 23 == 7 and last:
                content = {"msgtype": "m.text", "body": f"* правка {i}",
                           "m.new_content": {"msgtype": "m.text", "body": f"правка {i}"},
                           "m.relates_to": {"rel_type": "m.replace", "event_id": last}}
            else:
                content = f"{title}: сообщение {i} от {who}"
            last = send(tokens[who], room, content)
            events_sent += 1
            if i % 29 == 11:
                send(tokens[members[(i + 1) % len(members)]], room,
                     {"m.relates_to": {"rel_type": "m.annotation", "event_id": last, "key": "+1"}}, "m.reaction")
                events_sent += 1
            if i % 31 == 3:
                # redaction: свое сообщение - автором, каждое второе - модератором (alice)
                victim = send(tokens[who], room, f"удалю {i}")
                redactor = tokens[who] if (i // 31) % 2 == 0 else alice
                redact(redactor, room, victim, "репетиция")
                events_sent += 2
        state(alice, room, "m.room.topic", {"topic": title + ": тема 2"})

    # Выход участника из общей комнаты.
    call("POST", room_path(marks["rooms"]["general"]) + "/leave", tokens["zoe"], {})

    # Медиа: PNG разных размеров (с миниатюрами Synapse), бинарные, одно крупное.
    room = marks["rooms"]["general"]
    senders = people[:-1]  # zoe вышла из общей комнаты
    for i in range(20):
        side = 16 + (i * 37) % 300
        data = png(side, side, i + 1)
        who = senders[i % len(senders)]
        uri = upload(tokens[who], data, "image/png", f"pic{i}.png")
        marks["media"][uri.rsplit("/", 1)[1]] = hashlib.sha256(data).hexdigest()
        send(tokens[who], room, {"msgtype": "m.image", "body": f"pic{i}.png", "url": uri})
    for i in range(4):
        data = os.urandom(1024 * (i + 1))
        uri = upload(alice, data, "application/octet-stream", f"blob{i}.bin")
        marks["media"][uri.rsplit("/", 1)[1]] = hashlib.sha256(data).hexdigest()
    big = os.urandom(3 * 1024 * 1024)
    uri = upload(alice, big, "application/octet-stream", "big.bin")
    marks["media"][uri.rsplit("/", 1)[1]] = hashlib.sha256(big).hexdigest()

    # Карантин двух медиа (admin API).
    for media_id in list(marks["media"])[1:3]:
        call("POST", "/_synapse/admin/v1/media/quarantine/" + SERVER + "/" + media_id, alice, {})
        marks["quarantined"].append(media_id)

    # Превью ссылки: Synapse скачивает og:image в local_media_repository с url_cache.
    preview = call("GET", "/_matrix/client/v1/media/preview_url?url=" + urllib.parse.quote(PREVIEW_URL), alice)
    marks["preview"] = {k: v for k, v in preview.items() if k in ("og:title", "og:image")}

    # Деактивированный пользователь.
    call("POST", "/_synapse/admin/v1/deactivate/" + urllib.parse.quote("@walter:" + SERVER), alice, {"erase": False})

    marks["events_sent"] = events_sent
    print(json.dumps(marks, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
