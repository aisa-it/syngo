#!/usr/bin/env bash
# Сквозная проверка прод-деплоя: собирает образ, поднимает
# deploy/prod/docker-compose.yml в отдельном compose-проекте со сгенерированными
# секретами, прогоняет smoke.sh, проверяет сохранность медиа после рестарта и
# гасит проект вместе с томами.
#
#   deploy/prod/smoke-compose.sh            # проект syngo-smoke-<pid>, порт 18008
#   SMOKE_PORT=18108 KEEP=1 deploy/prod/smoke-compose.sh
#
# Чужие контейнеры не трогает: все ресурсы с префиксом проекта, удаляются в конце.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
COMPOSE_FILE=$ROOT/deploy/prod/docker-compose.yml
PROJECT=${SMOKE_PROJECT:-syngo-smoke-$$}
PORT=${SMOKE_PORT:-18008}
WORK=$(mktemp -d)
ENV_FILE=$WORK/.env

say() { echo "== $*"; }

rand() { head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'; }

SHARED_SECRET=$(rand 32)
cat >"$ENV_FILE" <<EOF
SYNGOD_SERVER_NAME=smoke.example.org
POSTGRES_PASSWORD=$(rand 24)
SYNGOD_SYNC_TOKEN_SECRET=$(rand 32)
SYNGOD_REGISTRATION_SHARED_SECRET=$SHARED_SECRET
SYNGOD_PORT=$PORT
SYNGO_IMAGE=syngo/syngod:smoke-$$
EOF

dc() { docker compose -p "$PROJECT" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"; }

cleanup() {
	rc=$?
	if [[ $rc != 0 ]]; then
		echo "--- логи (последние 50 строк)" >&2
		dc logs --no-color --tail=50 >&2 || true
	fi
	if [[ -n ${KEEP:-} ]]; then
		echo "проект $PROJECT оставлен (KEEP=1), env: $ENV_FILE"
		return
	fi
	dc down -v --remove-orphans >/dev/null 2>&1 || true
	docker image rm "syngo/syngod:smoke-$$" >/dev/null 2>&1 || true
	rm -f "$ENV_FILE"
	rmdir "$WORK" 2>/dev/null || true
	exit "$rc"
}
trap cleanup EXIT

# Сборка без --target: последняя стадия Dockerfile должна быть prod (distroless, nonroot).
say "docker build без --target"
docker build -q -t "syngo/syngod:smoke-$$" "$ROOT" >/dev/null
default_user=$(docker image inspect -f '{{.Config.User}}' "syngo/syngod:smoke-$$")
[[ $default_user == nonroot* || $default_user == 65532* ]] || { echo "образ запускается от '$default_user', ожидали nonroot" >&2; exit 1; }

say "up --wait (postgres -> migrate -> syngod)"
dc up -d --wait --wait-timeout 120

say "health контейнера: $(docker inspect -f '{{.State.Health.Status}}' "$(dc ps -q syngod)")"

BASE=http://127.0.0.1:$PORT SMOKE_SHARED_SECRET=$SHARED_SECRET "$ROOT/deploy/prod/smoke.sh"

say "рестарт syngod: миграции идемпотентны, схема совпадает"
dc up -d --wait --wait-timeout 60 --force-recreate syngod migrate
BASE=http://127.0.0.1:$PORT SMOKE_SHARED_SECRET=$SHARED_SECRET "$ROOT/deploy/prod/smoke.sh" >/dev/null

say "OK: прод-compose поднимается одной командой и проходит smoke"
