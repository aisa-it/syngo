#!/usr/bin/env bash
# Проверка бэкапа восстановлением: restore.sh во временную
# базу и временный каталог медиа, сверка с манифестом самого бэкапа, удаление.
#
#   SYNGOD_DATABASE_DSN=postgres://.../syngo ./deploy/verify-restore.sh /var/backups/syngo/syngo-<stamp>
#
# Живые база и каталог медиа не затрагиваются: создается база
# syngo_verify_<pid>_<rand> на том же сервере (нужно право CREATEDB) или на
# VERIFY_DSN - отдельном сервере для проверок; медиа распаковываются в mktemp.
# Счетчики сравниваются с counts.tsv из снимка бэкапа, а не с живой базой:
# события после дампа не дают ложный FAIL.
#
# Переменные: SYNGOD_DATABASE_DSN или VERIFY_DSN (сервер для временной базы),
# PSQL, PG_RESTORE (как в restore.sh), TMPDIR.
set -euo pipefail
umask 077

BACKUP=${1:-}
DSN=${VERIFY_DSN:-${SYNGOD_DATABASE_DSN:-}}
read -ra psql_cmd <<<"${PSQL:-psql}"

[[ -n $BACKUP && -e $BACKUP ]] || { echo "использование: verify-restore.sh <бэкап>" >&2; exit 1; }
[[ -n $DSN ]] || { echo "нужен VERIFY_DSN или SYNGOD_DATABASE_DSN" >&2; exit 1; }
command -v "${psql_cmd[0]}" >/dev/null || { echo "нет ${psql_cmd[0]}" >&2; exit 1; }

# DSN другой базы того же сервера: postgres://u:p@h:5432/<db>?opts.
with_db() {
	local re='^([^?]*/)([^/?]*)(\?.*)?$'
	[[ $DSN =~ $re ]] || { echo "не разобран DSN" >&2; exit 1; }
	printf '%s%s%s' "${BASH_REMATCH[1]}" "$1" "${BASH_REMATCH[3]}"
}

tmp_db="syngo_verify_$$_$RANDOM"
admin_dsn=$(with_db postgres)
tmp_dsn=$(with_db "$tmp_db")
work=$(mktemp -d)

cleanup() {
	"${psql_cmd[@]}" -X -q "$admin_dsn" -c "DROP DATABASE IF EXISTS $tmp_db WITH (FORCE)" >/dev/null 2>&1 || true
	rm -rf "$work"
}

trap cleanup EXIT

echo "==> временная база $tmp_db, медиа в $work/media"
"${psql_cmd[@]}" -X -q -v ON_ERROR_STOP=1 "$admin_dsn" -c "CREATE DATABASE $tmp_db"

# MEDIA_RESTORE_CMD и SECRETS_DIR сбрасываются: проверка пишет только во временные цели.
SYNGOD_DATABASE_DSN=$tmp_dsn MEDIA_DIR=$work/media MEDIA_RESTORE_CMD="" SECRETS_DIR="" RESTORE_FORCE=0 \
	TMPDIR=$work "$(dirname "$0")/restore.sh" "$BACKUP"

echo "OK: бэкап $(basename "$BACKUP") восстанавливается и совпадает со своим манифестом"
