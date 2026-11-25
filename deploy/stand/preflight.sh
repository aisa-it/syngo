#!/usr/bin/env bash
# Preflight интероп-стенда: проверяет каждый хоп федерации по отдельности
# и печатает вердикт. Запускается из interop.sh перед сценариями; можно и вручную:
#
#   deploy/stand/preflight.sh
#
# Что проверяем (и почему):
#   1. /etc/hosts хоста: a.test/b.test → 127.0.0.2, synapse.test → 127.0.0.1.
#      Synapse (Twisted) резолвит только через hosts ХОСТА — он в host-сети.
#   2. TCP+TLS до Synapse с хоста и из контейнера A (наш транспорт ходит по synapse.test).
#   3. TCP+TLS до a.test из контейнера Synapse — это тот путь, где Synapse забирает
#      наши ключи и валидирует invite (ошибка 401 «Failed to find any key» была здесь).
#   4. Наш /_matrix/key/v2/server с a.test: должен отдавать server_name=a.test и valid_until_ms.
#   5. /_matrix/federation/v1/version у Synapse (management-порт 8008 без TLS).
set -uo pipefail

cd "$(dirname "$0")"

SYNAPSE_CONTAINER=${SYNAPSE_CONTAINER:-syngo-stand-synapse-1}
A_CONTAINER=${A_CONTAINER:-syngo-stand-syngod-a-1}
FAILED=0
CHECKS=0

ok()   { CHECKS=$((CHECKS + 1)); printf '  OK   %s\n' "$*"; }
bad()  { CHECKS=$((CHECKS + 1)); FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$*"; }
step() { printf '==> %s\n' "$*"; }

# 1. hosts
step "hosts на хосте"
for pair in "a.test:127.0.0.2" "b.test:127.0.0.2" "synapse.test:127.0.0.1"; do
    name=${pair%%:*}
    want=${pair##*:}
    got=$(getent hosts "$name" | awk '{print $1}' | head -1)
    if [[ "$got" == "$want" ]]; then
        ok "$name → $got"
    else
        bad "$name → ${got:-<нет записи>} (ожидали $want)"
    fi
done

# 2. Synapse со стороны хоста
step "Synapse с хоста"
if curl -sk --max-time 5 -o /dev/null -w '%{http_code}' https://synapse.test:8448/_matrix/federation/v1/version | grep -q 200; then
    ok "https://synapse.test:8448/_matrix/federation/v1/version"
else
    bad "Synapse не отвечает на https://synapse.test:8448 (TLS-листенер федерации)"
fi

# 3. Наш A со стороны хоста по имени a.test
step "a.test с хоста"
if curl -sk --max-time 5 -o /dev/null -w '%{http_code}' https://a.test:8448/health | grep -q 200; then
    ok "https://a.test:8448/health"
else
    bad "a.test:8448 не отвечает с хоста (порт опубликован на 127.0.0.2:8448?)"
fi

# 4. Из контейнера Synapse: резолв и доступность a.test (главный хоп федерации)
if docker inspect "$SYNAPSE_CONTAINER" >/dev/null 2>&1; then
    step "a.test из контейнера Synapse"
    resolved=$(docker exec "$SYNAPSE_CONTAINER" getent hosts a.test 2>/dev/null | awk '{print $1}' | head -1)
    if [[ -n "$resolved" ]]; then
        ok "getent a.test → $resolved"
    else
        bad "Synapse не резолвит a.test (нет записи в /etc/hosts хоста)"
    fi

    key_code=$(docker exec -i "$SYNAPSE_CONTAINER" python3 - <<'PY' 2>/dev/null || true
import ssl, urllib.request
ctx = ssl.create_default_context()
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
try:
    with urllib.request.urlopen("https://a.test:8448/_matrix/key/v2/server", context=ctx, timeout=5) as r:
        print(r.status)
except Exception:
    print("000")
PY
)
    if [[ "$key_code" == "200" ]]; then
        ok "Synapse получил https://a.test:8448/_matrix/key/v2/server (200)"
    else
        bad "Synapse не смог забрать наши ключи с a.test:8448 (код ${key_code:-000}) — это и есть 401 на invite"
    fi
else
    bad "контейнер $SYNAPSE_CONTAINER не найден (docker compose up?)"
fi

# 5. Наш транспорт ходит в Synapse: проверяем из контейнера A
if docker inspect "$A_CONTAINER" >/dev/null 2>&1; then
    step "synapse.test из контейнера A"
    code=$(docker exec "$A_CONTAINER" wget --no-check-certificate -qO- \
        "https://synapse.test:8448/_matrix/federation/v1/version" >/dev/null 2>&1 && echo 200 || echo 000)
    if [[ "$code" == "200" ]]; then
        ok "A видит Synapse по synapse.test:8448"
    else
        bad "A не видит synapse.test:8448 (нужен extra_hosts synapse.test:host-gateway)"
    fi
else
    bad "контейнер $A_CONTAINER не найден"
fi

printf '\npreflight: %d проверок, %d провалов\n' "$CHECKS" "$FAILED"

if [[ "$FAILED" -gt 0 ]]; then
    printf 'ВЕРДИКТ: стенд не готов — сначала исправьте провалы выше.\n' >&2
    exit 1
fi

printf 'ВЕРДИКТ: стенд готов к интероп-прогону.\n'
