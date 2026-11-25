# Наполнение живого Synapse данными пользователей для фикстуры миграции.
#
# Запускается внутри контейнера matrixdotorg/synapse (там есть signedjson и nacl):
#   python /fixture/gen_fixture.py http://localhost:8008 <registration_shared_secret>
#
# Создает: устройства с именами, access и refresh токены, ключи устройств, one-time и
# fallback keys, кросс-подписи и подписи, бэкап ключей (удаленная и текущая версии),
# account_data (глобальная, комнатная, теги, секреты SSSS), push rules, pusher, профиль,
# фильтр, недоставленное to-device; флаги admin, deactivated, locked, shadow_banned;
# пользователя без пароля (как LDAP/SSO). Пароль alice - с лигатурой (проверка NFKC),
# сервер запущен с password_config.pepper.

import hashlib
import hmac
import json
import sys
import urllib.error
import urllib.request

import nacl.signing
import signedjson.key
import signedjson.sign
from unpaddedbase64 import encode_base64

BASE = sys.argv[1].rstrip("/")
SECRET = sys.argv[2]
SERVER = "fixture.local"
# "ﬁ" (U+FB01) после NFKC - "fi": Synapse хеширует нормализованный пароль.
ALICE_PASSWORD = "ﬁxture-Пароль-1"
PASSWORD = "fixture-password-1"


def call(method, path, body=None, token=None, ok=(200,)):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    try:
        with urllib.request.urlopen(req) as resp:
            code, raw = resp.status, resp.read()
    except urllib.error.HTTPError as err:
        code, raw = err.code, err.read()
    out = json.loads(raw or b"{}")
    if code not in ok:
        raise SystemExit(f"{method} {path}: {code} {out}")
    return out


def register(localpart, password, admin=False):
    nonce = call("GET", "/_synapse/admin/v1/register")["nonce"]
    mac = hmac.new(SECRET.encode(), digestmod=hashlib.sha1)
    mac.update(f"{nonce}\x00{localpart}\x00{password}\x00{'admin' if admin else 'notadmin'}".encode())
    return call("POST", "/_synapse/admin/v1/register", {
        "nonce": nonce, "username": localpart, "password": password,
        "admin": admin, "mac": mac.hexdigest(),
    })


def login(localpart, password, device_id, name, refresh=False):
    return call("POST", "/_matrix/client/v3/login", {
        "type": "m.login.password",
        "identifier": {"type": "m.id.user", "user": localpart},
        "password": password, "device_id": device_id,
        "initial_device_display_name": name, "refresh_token": refresh,
    })


def device_keys(user_id, device_id):
    sk = nacl.signing.SigningKey.generate()
    signing = signedjson.key.decode_signing_key_base64("ed25519", device_id, encode_base64(bytes(sk)))
    curve = encode_base64(nacl.public.PrivateKey.generate().public_key.encode())
    obj = {
        "user_id": user_id, "device_id": device_id,
        "algorithms": ["m.olm.v1.curve25519-aes-sha2", "m.megolm.v1.aes-sha2"],
        "keys": {
            "curve25519:" + device_id: curve,
            "ed25519:" + device_id: encode_base64(bytes(sk.verify_key)),
        },
    }
    return signedjson.sign.sign_json(obj, user_id, signing), signing


def signed_otk(user_id, signing, n):
    pub = encode_base64(nacl.public.PrivateKey.generate().public_key.encode())
    return signedjson.sign.sign_json({"key": pub}, user_id, signing)


def cross_key(user_id, usage):
    sk = nacl.signing.SigningKey.generate()
    pub = encode_base64(bytes(sk.verify_key))
    signing = signedjson.key.decode_signing_key_base64("ed25519", pub, encode_base64(bytes(sk)))
    return {"user_id": user_id, "usage": [usage], "keys": {"ed25519:" + pub: pub}}, signing, pub


def setup_cross_signing(user_id, token, password):
    master, master_sk, master_pub = cross_key(user_id, "master")
    ssk, ssk_sk, ssk_pub = cross_key(user_id, "self_signing")
    usk, usk_sk, usk_pub = cross_key(user_id, "user_signing")
    ssk = signedjson.sign.sign_json(ssk, user_id, master_sk)
    usk = signedjson.sign.sign_json(usk, user_id, master_sk)
    body = {"master_key": master, "self_signing_key": ssk, "user_signing_key": usk}
    res = call("POST", "/_matrix/client/v3/keys/device_signing/upload", body, token, ok=(200, 401))
    if "flows" in res:
        body["auth"] = {"type": "m.login.password", "session": res.get("session"),
                        "identifier": {"type": "m.id.user", "user": user_id}, "password": password}
        call("POST", "/_matrix/client/v3/keys/device_signing/upload", body, token)
    return {"master": (master, master_sk, master_pub), "ssk": (ssk_sk, ssk_pub), "usk": (usk_sk, usk_pub)}


def strip(obj):
    return {k: v for k, v in obj.items() if k not in ("signatures", "unsigned")}


def main():
    alice_id, bob_id = f"@alice:{SERVER}", f"@bob:{SERVER}"

    # Пользователи и флаги.
    register("alice", ALICE_PASSWORD)
    register("bob", PASSWORD)
    carol = register("carol", PASSWORD, admin=True)
    register("dave", PASSWORD)
    register("eve", PASSWORD)
    register("frank", PASSWORD)
    admin = carol["access_token"]
    call("POST", f"/_synapse/admin/v1/deactivate/@dave:{SERVER}", {"erase": False}, admin)
    call("PUT", f"/_synapse/admin/v2/users/@eve:{SERVER}", {"locked": True}, admin)
    call("POST", f"/_synapse/admin/v1/users/@frank:{SERVER}/shadow_ban", {}, admin)
    # Пользователь без пароля (так выглядят учетки LDAP и SSO): password_hash IS NULL.
    call("PUT", f"/_synapse/admin/v2/users/@ldapuser:{SERVER}", {"displayname": "LDAP User"}, admin, ok=(200, 201))

    # Устройства alice: ALICEPHONE с refresh-токеном, ALICELAPTOP - бессрочный токен.
    phone = login("alice", ALICE_PASSWORD, "ALICEPHONE", "Alice phone", refresh=True)
    laptop = login("alice", ALICE_PASSWORD, "ALICELAPTOP", "Alice laptop")
    bobdev = login("bob", PASSWORD, "BOBDEV", "Bob desktop")
    at, bt = laptop["access_token"], bobdev["access_token"]

    # Активность устройства (last_seen, ip).
    call("GET", "/_matrix/client/v3/account/whoami", token=at)

    # Ключи устройств, OTK и fallback.
    keys = {}
    for dev, tok, uid in (("ALICEPHONE", phone["access_token"], alice_id), ("ALICELAPTOP", at, alice_id),
                          ("BOBDEV", bt, bob_id)):
        obj, signing = device_keys(uid, dev)
        keys[dev] = (obj, signing)
        otks = {f"signed_curve25519:{dev}OTK{i}": signed_otk(uid, signing, i) for i in range(3)}
        fallback = signed_otk(uid, signing, 99)
        fallback["fallback"] = True
        call("POST", "/_matrix/client/v3/keys/upload", {
            "device_keys": obj, "one_time_keys": otks,
            "fallback_keys": {f"signed_curve25519:{dev}FB": fallback},
        }, tok)
    # bob забирает один OTK alice: в Synapse он удаляется, в дампе их два.
    call("POST", "/_matrix/client/v3/keys/claim",
         {"one_time_keys": {alice_id: {"ALICELAPTOP": "signed_curve25519"}}}, bt)

    # Кросс-подписи и подписи.
    acs = setup_cross_signing(alice_id, at, ALICE_PASSWORD)
    bcs = setup_cross_signing(bob_id, bt, PASSWORD)
    laptop_obj = signedjson.sign.sign_json(strip(keys["ALICELAPTOP"][0]), alice_id, acs["ssk"][0])
    master_obj = signedjson.sign.sign_json(strip(acs["master"][0]), alice_id, keys["ALICELAPTOP"][1])
    bob_master = signedjson.sign.sign_json(strip(bcs["master"][0]), alice_id, acs["usk"][0])
    res = call("POST", "/_matrix/client/v3/keys/signatures/upload", {
        alice_id: {"ALICELAPTOP": laptop_obj, acs["master"][2]: master_obj},
        bob_id: {bcs["master"][2]: bob_master},
    }, at)
    if res.get("failures"):
        raise SystemExit(f"signatures failures: {res}")
    bob_dev = signedjson.sign.sign_json(strip(keys["BOBDEV"][0]), bob_id, bcs["ssk"][0])
    call("POST", "/_matrix/client/v3/keys/signatures/upload", {bob_id: {"BOBDEV": bob_dev}}, bt)

    # Комната alice и bob.
    # Версия 10: room_id с доменом (у v12 его нет - перенос таких комнат вне этой фикстуры).
    room = call("POST", "/_matrix/client/v3/createRoom", {"name": "Fixture", "invite": [bob_id],
                                                          "room_version": "10"}, at)["room_id"]
    call("POST", f"/_matrix/client/v3/join/{room}", {}, bt)
    call("PUT", f"/_matrix/client/v3/directory/room/%23fixture:{SERVER}", {"room_id": room}, at)
    call("PUT", f"/_matrix/client/v3/rooms/{room}/send/m.room.message/m1", {"msgtype": "m.text", "body": "до переезда"}, at)
    call("PUT", f"/_matrix/client/v3/rooms/{room}/send/m.room.encrypted/m2", {
        "algorithm": "m.megolm.v1.aes-sha2", "sender_key": "c2s", "ciphertext": "Y2lwaGVy",
        "session_id": "session0", "device_id": "ALICELAPTOP"}, at)

    # Бэкап ключей: версия 1 удалена, версия 2 текущая с двумя сессиями.
    auth_data = {"public_key": encode_base64(nacl.public.PrivateKey.generate().public_key.encode())}
    algo = "m.megolm_backup.v1.curve25519-aes-sha2"
    v1 = call("POST", "/_matrix/client/v3/room_keys/version", {"algorithm": algo, "auth_data": auth_data}, at)["version"]
    call("DELETE", f"/_matrix/client/v3/room_keys/version/{v1}", None, at)
    v2 = call("POST", "/_matrix/client/v3/room_keys/version", {"algorithm": algo, "auth_data": auth_data}, at)["version"]
    sessions = {}
    for i in range(2):
        sessions[f"session{i}"] = {
            "first_message_index": i, "forwarded_count": 0, "is_verified": i == 0,
            "session_data": {"ephemeral": f"eph{i}", "ciphertext": f"ct{i}", "mac": f"mac{i}"},
        }
    call("PUT", f"/_matrix/client/v3/room_keys/keys?version={v2}", {"rooms": {room: {"sessions": sessions}}}, at)

    # account_data: глобальная (в том числе секреты SSSS), комнатная, теги.
    for typ, content in (
        ("m.direct", {bob_id: [room]}),
        ("m.secret_storage.default_key", {"key": "SSSSKEY"}),
        ("m.secret_storage.key.SSSSKEY", {"algorithm": "m.secret_storage.v1.aes-hmac-sha2", "iv": "aXY", "mac": "bWFj"}),
        ("m.cross_signing.master", {"encrypted": {"SSSSKEY": {"iv": "aXY", "ciphertext": "Y3Q", "mac": "bWFj"}}}),
        ("m.megolm_backup.v1", {"encrypted": {"SSSSKEY": {"iv": "aXY", "ciphertext": "Ymt1cA", "mac": "bWFj"}}}),
        ("im.vector.setting.breadcrumbs", {"recent_rooms": [room]}),
    ):
        call("PUT", f"/_matrix/client/v3/user/{alice_id}/account_data/{typ}", content, at)
    call("PUT", f"/_matrix/client/v3/user/{alice_id}/rooms/{room}/account_data/org.example.room.note", {"note": "заметка"}, at)
    call("PUT", f"/_matrix/client/v3/user/{alice_id}/rooms/{room}/tags/m.favourite", {"order": 0.5}, at)
    call("PUT", f"/_matrix/client/v3/user/{alice_id}/rooms/{room}/tags/u.work", {}, at)

    # Push rules: два своих override (порядок важен), content с pattern, комната без
    # звука, выключенное и измененное серверные правила.
    call("PUT", "/_matrix/client/v3/pushrules/global/override/second", {
        "conditions": [{"kind": "event_match", "key": "type", "pattern": "m.second"}], "actions": []}, at)
    call("PUT", "/_matrix/client/v3/pushrules/global/override/first?before=second", {
        "conditions": [{"kind": "event_match", "key": "type", "pattern": "m.first"}], "actions": ["notify"]}, at)
    call("PUT", "/_matrix/client/v3/pushrules/global/content/coffee", {
        "pattern": "coffee", "actions": ["notify", {"set_tweak": "highlight"}]}, at)
    call("PUT", f"/_matrix/client/v3/pushrules/global/room/{room}", {"actions": []}, at)
    call("PUT", "/_matrix/client/v3/pushrules/global/override/.m.rule.invite_for_me/enabled", {"enabled": False}, at)
    call("PUT", "/_matrix/client/v3/pushrules/global/underride/.m.rule.message/actions", {"actions": []}, at)
    call("PUT", "/_matrix/client/v3/pushrules/global/override/first/enabled", {"enabled": False}, at)

    # Pusher устройства ALICELAPTOP.
    call("POST", "/_matrix/client/v3/pushers/set", {
        "kind": "http", "app_id": "im.vector.app.android", "pushkey": "pushkey-alice-1",
        "app_display_name": "Element", "device_display_name": "Alice laptop", "lang": "ru",
        "data": {"url": "https://push.example.org/_matrix/push/v1/notify", "format": "event_id_only"},
    }, at)

    # Профиль и фильтр.
    call("PUT", f"/_matrix/client/v3/profile/{alice_id}/displayname", {"displayname": "Алиса"}, at)
    call("PUT", f"/_matrix/client/v3/profile/{alice_id}/avatar_url", {"avatar_url": f"mxc://{SERVER}/avatar1"}, at)
    call("POST", f"/_matrix/client/v3/user/{alice_id}/filter", {"room": {"timeline": {"limit": 7}}}, at)

    # Недоставленное to-device для ALICEPHONE (устройство не синхронизировалось).
    call("PUT", "/_matrix/client/v3/sendToDevice/m.room_key_request/txn1", {
        "messages": {alice_id: {"ALICEPHONE": {"action": "request", "request_id": "req1",
                                              "requesting_device_id": "BOBDEV"}}}}, bt)

    print(json.dumps({"room_id": room, "alice_laptop_token": at, "alice_phone_refresh": phone.get("refresh_token")}))


if __name__ == "__main__":
    main()
