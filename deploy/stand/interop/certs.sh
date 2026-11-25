#!/usr/bin/env bash
# Самоподписанный CA и сертификаты для интероп-стенда: a.test, b.test, synapse.test.
set -euo pipefail

cd "$(dirname "$0")/.."

mkdir -p certs
chmod 700 certs

# CA обязан иметь basicConstraints=CA:TRUE и keyUsage/keyCertSign: OpenSSL 3 (и Python 3.13
# в Synapse) отвергает цепочку без этих расширений — «CA cert does not include key usage extension».
if [[ ! -f certs/ca.key ]]; then
	openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
		-keyout certs/ca.key -out certs/ca.crt \
		-subj "/CN=SYNGO stand CA" \
		-addext "basicConstraints=critical,CA:TRUE" \
		-addext "keyUsage=critical,keyCertSign,cRLSign" \
		-addext "subjectKeyIdentifier=hash"
	chmod 600 certs/ca.key
fi

for name in a.test b.test synapse.test; do
	[[ -f certs/$name.key ]] && continue

	openssl req -newkey rsa:2048 -nodes -keyout "certs/$name.key" -out "certs/$name.csr" \
		-subj "/CN=$name"
	openssl x509 -req -in "certs/$name.csr" -CA certs/ca.crt -CAkey certs/ca.key -CAcreateserial \
		-days 365 -out "certs/$name.crt" \
		-extfile <(printf 'subjectAltName=DNS:%s\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nauthorityKeyIdentifier=keyid,issuer\n' "$name")
	rm -f "certs/$name.csr"
	chmod 600 "certs/$name.key"
done

echo "сертификаты готовы: $(ls certs/*.crt | tr '\n' ' ')"
