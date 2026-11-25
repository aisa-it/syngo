# Наполнение живого Synapse комнатами версии 12 для фикстуры миграции.
#
# Запускается внутри контейнера matrixdotorg/synapse (через gen.sh, SCENARIO=v12):
#   python /data/gen_fixture_v12.py http://localhost:8008 <registration_shared_secret>
#
# В комнатах v12 room_id - хеш m.room.create без серверной части, создатели (отправитель
# create и additional_creators) не записаны в power_levels и имеют бесконечный уровень,
# create не входит в auth_events. Сценарии:
#   - «V12»: версия по умолчанию Synapse 1.165 (12), alice - создатель, bob - дополнительный
#     создатель, carol получает уровень 50 и меняет тему, сообщения, redaction чужого
#     сообщения создателем, алиас, приглашение dave без ответа;
#   - «Upgraded»: комната v11, замененная на v12 (/upgrade): tombstone и predecessor.

import json
import sys

from gen_fixture import SERVER, call, login, register

PASSWORD = "fixture-password-1"


def create_version(room, token):
    return call("GET", f"/_matrix/client/v3/rooms/{room}/state/m.room.create", token=token)["room_version"]


def send(room, txn, body, token):
    return call("PUT", f"/_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}",
                {"msgtype": "m.text", "body": body}, token)["event_id"]


def main():
    alice_id, bob_id, carol_id, dave_id = (f"@{u}:{SERVER}" for u in ("alice", "bob", "carol", "dave"))

    for user in ("alice", "bob", "carol", "dave"):
        register(user, PASSWORD)

    at = login("alice", PASSWORD, "ALICEDEV", "Alice desktop")["access_token"]
    bt = login("bob", PASSWORD, "BOBDEV", "Bob desktop")["access_token"]
    ct = login("carol", PASSWORD, "CAROLDEV", "Carol desktop")["access_token"]
    login("dave", PASSWORD, "DAVEDEV", "Dave desktop")

    # Комната версии по умолчанию: room_version не передается.
    room = call("POST", "/_matrix/client/v3/createRoom", {
        "name": "Комната v12", "topic": "до переезда", "preset": "private_chat",
        "invite": [bob_id, carol_id],
        "creation_content": {"additional_creators": [bob_id]},
    }, at)["room_id"]
    if ":" in room or create_version(room, at) != "12":
        raise SystemExit(f"ожидали комнату v12 без домена: {room}")

    call("POST", f"/_matrix/client/v3/join/{room}", {}, bt)
    call("POST", f"/_matrix/client/v3/join/{room}", {}, ct)
    call("PUT", f"/_matrix/client/v3/directory/room/%23v12:{SERVER}", {"room_id": room}, at)
    send(room, "a1", "привет из v12", at)
    send(room, "b1", "ответ дополнительного создателя", bt)

    # Создатели не записаны в users; alice выдает carol уровень 50.
    levels = call("GET", f"/_matrix/client/v3/rooms/{room}/state/m.room.power_levels", token=at)
    levels.setdefault("users", {})[carol_id] = 50
    call("PUT", f"/_matrix/client/v3/rooms/{room}/state/m.room.power_levels", levels, at)
    call("PUT", f"/_matrix/client/v3/rooms/{room}/state/m.room.topic", {"topic": "тема от carol"}, ct)

    secret = send(room, "c1", "лишнее сообщение carol", ct)
    call("PUT", f"/_matrix/client/v3/rooms/{room}/redact/{secret}/r1", {"reason": "модерация"}, bt)
    send(room, "c2", "после модерации", ct)
    call("POST", f"/_matrix/client/v3/rooms/{room}/invite", {"user_id": dave_id}, at)

    # Комната v11, замененная на v12.
    old = call("POST", "/_matrix/client/v3/createRoom", {
        "name": "Upgraded", "preset": "private_chat", "invite": [bob_id], "room_version": "11",
    }, at)["room_id"]
    call("POST", f"/_matrix/client/v3/join/{old}", {}, bt)
    send(old, "u1", "до замены", at)
    new = call("POST", f"/_matrix/client/v3/rooms/{old}/upgrade", {"new_version": "12"}, at)["replacement_room"]
    if ":" in new or create_version(new, at) != "12":
        raise SystemExit(f"upgrade не дал v12: {new}")

    call("POST", f"/_matrix/client/v3/rooms/{new}/invite", {"user_id": bob_id}, at)
    call("POST", f"/_matrix/client/v3/join/{new}", {}, bt)
    send(new, "u2", "после замены", bt)

    print(json.dumps({"room_v12": room, "room_v11": old, "room_upgraded": new}))


if __name__ == "__main__":
    main()
