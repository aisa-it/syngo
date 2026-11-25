#!/usr/bin/env bash
# Самоподписанный сертификат для Element Web. Нужен потому, что Element
# требует secure context: без HTTPS браузер не даёт crypto.randomUUID, и клиент
# зависает на «Неподдерживаемый браузер».
#
#   deploy/stand/element/gen-cert.sh [IP]
set -euo pipefail

ip=${1:-192.0.2.10}
dir=$(cd "$(dirname "$0")" && pwd)/tls
mkdir -p "$dir"

openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
    -keyout "$dir/element.key" -out "$dir/element.crt" \
    -subj "/CN=$ip" \
    -addext "subjectAltName=IP:$ip,DNS:localhost" >/dev/null 2>&1

# 644: nginx в контейнере Element работает не от root и иначе не читает ключ.
chmod 644 "$dir/element.key"
echo "сертификат: $dir/element.crt (SAN IP:$ip)"
