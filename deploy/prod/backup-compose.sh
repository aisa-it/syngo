#!/usr/bin/env bash
# Бэкап, проверка и восстановление продового compose: обертка над
# deploy/backup.sh, verify-restore.sh и restore.sh. PostgreSQL - через
# `docker compose exec postgres` (порт наружу не публикуется), том медиа -
# через одноразовый контейнер из образа postgres. Runbook - docs/operations.md.
#
#   deploy/prod/backup-compose.sh backup                  # бэкап в BACKUP_DIR
#   deploy/prod/backup-compose.sh verify <бэкап>          # временная база + mktemp
#   deploy/prod/backup-compose.sh restore <бэкап>         # в этот compose-проект
#
# Переменные: COMPOSE_PROJECT_NAME или COMPOSE_PROJECT (по умолчанию - из
# compose-файла), COMPOSE_ENV_FILE (deploy/prod/.env), BACKUP_DIR, KEEP,
# BACKUP_GPG_RECIPIENT, BACKUP_UPLOAD_CMD, BACKUP_METRICS_FILE (как в backup.sh);
# BACKUP_SECRET_FILES по умолчанию - .env и config.yaml рядом с compose-файлом.
# restore: syngod останавливается; том медиа и база должны быть пустыми
# (новый проект или после `down -v`), иначе RESTORE_FORCE=1.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
COMPOSE_FILE=${COMPOSE_FILE:-$ROOT/deploy/prod/docker-compose.yml}
ENV_FILE=${COMPOSE_ENV_FILE:-$ROOT/deploy/prod/.env}
HELPER_IMAGE=${HELPER_IMAGE:-postgres:17-alpine}

cmd=${1:-}
shift || true

die() {
	echo "FAIL: $*" >&2
	exit 1
}

dc_args=(-f "$COMPOSE_FILE")
[[ -f $ENV_FILE ]] && dc_args+=(--env-file "$ENV_FILE")
[[ -n ${COMPOSE_PROJECT:-} ]] && dc_args+=(-p "$COMPOSE_PROJECT")

dc() { docker compose "${dc_args[@]}" "$@"; }

# Строка команды для backup.sh/restore.sh (разбивается по пробелам): пути без пробелов.
dc_cmd="docker compose ${dc_args[*]}"

project=$(dc config | awk '/^name:/ { print $2; exit }')
[[ -n $project ]] || die "не определено имя compose-проекта"

pg_user=$(dc exec -T postgres printenv POSTGRES_USER) || die "postgres проекта $project не запущен"
pg_db=$(dc exec -T postgres printenv POSTGRES_DB)

# Внутри контейнера postgres - подключение по unix-сокету (локальный trust):
# пароль не попадает в аргументы процессов.
export SYNGOD_DATABASE_DSN="postgresql:///$pg_db?host=/var/run/postgresql&user=$pg_user"
export PSQL="$dc_cmd exec -T postgres psql"
export PG_DUMP="$dc_cmd exec -T postgres pg_dump"
export PG_RESTORE="$dc_cmd exec -T postgres pg_restore"

# Том монтируется в /syngo-media: каталога нет в образе-помощнике, поэтому
# docker не копирует в пустой том его содержимое (в alpine /media - cdrom, usb).
media_volume=$(docker volume ls -q \
	--filter "label=com.docker.compose.project=$project" \
	--filter "label=com.docker.compose.volume=media")
[[ -n $media_volume ]] || die "нет тома media проекта $project (сначала docker compose up)"

case $cmd in
backup)
	if [[ -z ${BACKUP_SECRET_FILES+x} ]]; then
		BACKUP_SECRET_FILES="$ROOT/deploy/prod/config.yaml"
		[[ -f $ENV_FILE ]] && BACKUP_SECRET_FILES="$ENV_FILE $BACKUP_SECRET_FILES"
		export BACKUP_SECRET_FILES
	fi

	# Том только на чтение; временные .upload-* не попадают в архив.
	export MEDIA_TAR_CMD="docker run --rm --network none -v $media_volume:/syngo-media:ro $HELPER_IMAGE tar -czf - -C /syngo-media --exclude='.upload-*' ."
	exec "$ROOT/deploy/backup.sh"
	;;
verify)
	[[ -n ${1:-} ]] || die "использование: backup-compose.sh verify <бэкап>"
	exec "$ROOT/deploy/verify-restore.sh" "$1"
	;;
restore)
	[[ -n ${1:-} ]] || die "использование: backup-compose.sh restore <бэкап>"

	echo "==> останавливаю syngod проекта $project"
	dc stop syngod >/dev/null 2>&1 || true

	if [[ ${RESTORE_FORCE:-0} != 1 ]]; then
		left=$(docker run --rm --network none -v "$media_volume:/syngo-media:ro" "$HELPER_IMAGE" \
			sh -c 'ls -A /syngo-media | wc -l')
		[[ $left == 0 ]] || die "том медиа $media_volume не пуст ($left записей): новый проект, down -v или RESTORE_FORCE=1"
	fi

	# Распаковка в том от root, затем владелец - nonroot образа syngod (65532).
	export MEDIA_RESTORE_CMD="docker run --rm -i --network none -v $media_volume:/syngo-media $HELPER_IMAGE sh -c 'tar -xzf - -C /syngo-media && chown -R 65532:65532 /syngo-media'"
	"$ROOT/deploy/restore.sh" "$1"

	echo "Дальше: docker compose ${dc_args[*]} up -d --wait (migrate догонит схему, если образ новее)"
	;;
*)
	die "использование: backup-compose.sh backup|verify <бэкап>|restore <бэкап>"
	;;
esac
