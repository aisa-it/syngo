#!/usr/bin/env bash
# Восстановление SYNGO из бэкапа backup.sh. Runbook -
# docs/operations.md, раздел «Восстановление».
#
#   SYNGOD_DATABASE_DSN=postgres://.../syngo MEDIA_DIR=/var/syngo/media \
#   ./deploy/restore.sh /var/backups/syngo/syngo-<stamp>[.tar.gpg]
#
# Порядок: расшифровка (для .tar.gpg) -> проверка SHA256SUMS всех файлов до
# любых изменений -> pg_restore одной транзакцией -> сверка счетчиков таблиц и
# версии схемы с манифестом бэкапа -> распаковка медиа во временный каталог,
# сверка файлов с media-keys.tsv (наличие и sha256) -> перенос в MEDIA_DIR ->
# секреты в SECRETS_DIR (если задан).
#
# База и каталог медиа должны быть пустыми; RESTORE_FORCE=1 - перезаписать базу
# (pg_restore --clean) и отложить прежние файлы медиа в MEDIA_DIR.pre-restore-<stamp>.
#
# Переменные:
#   SYNGOD_DATABASE_DSN  целевая база (обязательно)
#   MEDIA_DIR            целевой каталог медиа
#   MEDIA_RESTORE_CMD    вместо MEDIA_DIR: команда, читающая проверенный tar.gz
#                        медиа из stdin (том docker, deploy/prod/backup-compose.sh)
#   SECRETS_DIR          куда положить secrets/ из бэкапа (0600); без него -
#                        только путь в выводе
#   RESTORE_FORCE=1      восстановить поверх непустых базы и каталога медиа
#   PSQL, PG_RESTORE     команды psql и pg_restore (по умолчанию из PATH)
#   TMPDIR               временный каталог (расшифровка, распаковка медиа)
set -euo pipefail
umask 077

BACKUP=${1:-}
DSN=${SYNGOD_DATABASE_DSN:-}
MEDIA_DIR=${MEDIA_DIR:-}
MEDIA_RESTORE_CMD=${MEDIA_RESTORE_CMD:-}
SECRETS_DIR=${SECRETS_DIR:-}
FORCE=${RESTORE_FORCE:-0}
read -ra psql_cmd <<<"${PSQL:-psql}"
read -ra pg_restore_cmd <<<"${PG_RESTORE:-pg_restore}"

tmp=""
staging=""

die() {
	echo "FAIL: $*" >&2
	exit 1
}

cleanup() {
	[[ -n $staging && -d $staging ]] && rm -rf "$staging"
	[[ -n $tmp && -d $tmp ]] && rm -rf "$tmp"

	return 0
}

trap cleanup EXIT

[[ -n $BACKUP ]] || die "использование: restore.sh <каталог syngo-<stamp> или syngo-<stamp>.tar.gpg>"
[[ -n $DSN ]] || die "нужен SYNGOD_DATABASE_DSN"
command -v "${psql_cmd[0]}" >/dev/null || die "нет ${psql_cmd[0]}"
command -v "${pg_restore_cmd[0]}" >/dev/null || die "нет ${pg_restore_cmd[0]}"

if [[ $BACKUP == *.dump ]]; then
	die "$BACKUP - старый формат backup.sh (db-<stamp>.dump): восстановление вручную, docs/operations.md"
fi

if [[ -f $BACKUP && $BACKUP == *.tar.gpg ]]; then
	command -v gpg >/dev/null || die "нет gpg для расшифровки $BACKUP"
	echo "==> расшифровка $BACKUP"
	tmp=$(mktemp -d)
	gpg --batch --decrypt "$BACKUP" | tar -xf - -C "$tmp"
	dir=$(find "$tmp" -mindepth 1 -maxdepth 1 -type d -name 'syngo-*' | head -n1)
	[[ -n $dir ]] || die "в $BACKUP нет каталога syngo-<stamp>"
else
	dir=$BACKUP
fi

[[ -d $dir && -f $dir/manifest.txt ]] || die "$dir - не каталог бэкапа (нет manifest.txt)"

manifest() {
	awk -F= -v k="$1" '$1 == k { sub(/^[^=]*=/, ""); print; exit }' "$dir/manifest.txt"
}

[[ $(manifest format) == "syngo-backup/2" ]] || die "неизвестный формат бэкапа: $(manifest format)"
stamp=$(manifest stamp)

# Целостность - до любых изменений в базе и каталоге медиа.
echo "==> контрольные суммы ($dir)"
(cd "$dir" && sha256sum --quiet -c SHA256SUMS) || die "контрольные суммы не сошлись"

psql_value() {
	"${psql_cmd[@]}" -X -q -A -t -v ON_ERROR_STOP=1 "$DSN" -c "$1"
}

tables=$(psql_value "SELECT count(*) FROM pg_tables WHERE schemaname = 'public'")

if [[ $tables != 0 && $FORCE != 1 ]]; then
	die "в целевой базе $tables таблиц: нужна пустая база (или RESTORE_FORCE=1)"
fi

case $(manifest media) in
included) media_included=1 ;;
absent)
	[[ ! -s $dir/media-keys.tsv ]] || die "в снимке БД есть медиа, но в бэкапе нет архива медиа"
	media_included=0
	;;
*) die "неизвестное значение media в манифесте" ;;
esac

if [[ $media_included == 1 && -z $MEDIA_RESTORE_CMD ]]; then
	[[ -n $MEDIA_DIR ]] || die "в бэкапе есть медиа: задайте MEDIA_DIR или MEDIA_RESTORE_CMD"

	if [[ -d $MEDIA_DIR && -n $(ls -A "$MEDIA_DIR") && $FORCE != 1 ]]; then
		die "каталог медиа $MEDIA_DIR не пуст (или RESTORE_FORCE=1)"
	fi
fi

# Медиа проверяются до восстановления базы: битый архив не оставит
# восстановленную базу без файлов.
if [[ $media_included == 1 ]]; then
	if [[ -n $MEDIA_DIR && -z $MEDIA_RESTORE_CMD ]]; then
		mkdir -p "$(dirname "$MEDIA_DIR")"
		staging=$(mktemp -d "$(dirname "$MEDIA_DIR")/.restore-$stamp.XXXXXX")
	else
		staging=$(mktemp -d)
	fi

	echo "==> распаковка медиа во временный каталог $staging"
	tar -xzf "$dir/media.tar.gz" -C "$staging"

	echo "==> сверка медиа со снимком БД ($(manifest media_files) файлов)"
	missing=0

	while IFS=$'\t' read -r key _; do
		[[ -f $staging/$key ]] && continue
		echo "нет файла медиа: $key" >&2
		missing=$((missing + 1))
	done <"$dir/media-keys.tsv"

	[[ $missing == 0 ]] || die "в архиве медиа нет $missing файлов из снимка БД"

	# Сверяем все доступные в снимке хеши, включая записи старых копий формата v2.
	sums=$(mktemp)
	awk -F'\t' '$2 != "" { print $2 "  " $1 }' "$dir/media-keys.tsv" >"$sums"

	if [[ -s $sums ]]; then
		(cd "$staging" && sha256sum --quiet -c "$sums") || { rm -f "$sums"; die "sha256 файлов медиа не совпадает с БД"; }
	fi

	rm -f "$sums"
fi

echo "==> восстановление БД"
restore_args=(--no-owner --no-privileges --exit-on-error --single-transaction)

if [[ $tables != 0 ]]; then
	restore_args+=(--clean --if-exists)
fi

"${pg_restore_cmd[@]}" "${restore_args[@]}" --dbname "$DSN" <"$dir/db.dump"

echo "==> сверка счетчиков с манифестом бэкапа"
mismatch=0

while IFS=$'\t' read -r table want; do
	got=$(psql_value "SELECT count(*) FROM public.\"$table\"")

	if [[ $got != "$want" ]]; then
		echo "FAIL: $table: в бэкапе $want, восстановлено $got" >&2
		mismatch=$((mismatch + 1))
	fi
done <"$dir/counts.tsv"

[[ $mismatch == 0 ]] || die "счетчики не совпали в $mismatch таблицах"

schema=$(psql_value "SELECT version || ' ' || dirty FROM schema_migrations")
[[ $schema == "$(manifest schema_version) $(manifest schema_dirty)" ]] ||
	die "версия схемы $schema, в бэкапе $(manifest schema_version) $(manifest schema_dirty)"

if [[ $media_included == 1 ]]; then
	if [[ -n $MEDIA_RESTORE_CMD ]]; then
		echo "==> медиа в MEDIA_RESTORE_CMD"
		tar -czf - -C "$staging" . | bash -c "$MEDIA_RESTORE_CMD"
	else
		echo "==> медиа в $MEDIA_DIR"
		mkdir -p "$MEDIA_DIR"
		shopt -s dotglob nullglob

		existing=("$MEDIA_DIR"/*)

		if ((${#existing[@]} > 0)); then
			aside="$MEDIA_DIR.pre-restore-$stamp"
			mkdir -p "$aside"
			mv -- "${existing[@]}" "$aside"/
			echo "    прежние файлы медиа отложены в $aside"
		fi

		restored=("$staging"/*)
		((${#restored[@]} == 0)) || mv -- "${restored[@]}" "$MEDIA_DIR"/
		shopt -u dotglob nullglob
	fi
fi

if [[ -d $dir/secrets ]]; then
	if [[ -n $SECRETS_DIR ]]; then
		mkdir -p "$SECRETS_DIR"

		for file in "$dir"/secrets/*; do
			target="$SECRETS_DIR/$(basename "$file")"
			[[ -e $target && $FORCE != 1 ]] && die "$target уже существует (или RESTORE_FORCE=1)"
			install -m 600 -- "$file" "$target"
		done

		echo "==> секреты и конфиг в $SECRETS_DIR: $(manifest secrets)"
	else
		echo "==> секреты и конфиг в бэкапе: $dir/secrets ($(manifest secrets)); SECRETS_DIR не задан"
	fi
fi

echo "OK: восстановлено из $(basename "$dir") (схема $schema, таблиц $(wc -l <"$dir/counts.tsv"), медиа $(manifest media_files))."
echo "Дальше: запустить syngod и проверить /health/ready."
