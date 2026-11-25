#!/usr/bin/env bash
# Бэкап SYNGO: дамп PostgreSQL и медиа одним снимком,
# секреты и конфиг, манифест счетчиков, контрольные суммы, шифрование, вынос,
# ротация и метрики для алертов. Runbook - docs/operations.md.
#
#   SYNGOD_DATABASE_DSN=postgres://... MEDIA_DIR=/var/syngo/media \
#   BACKUP_SECRET_FILES="/etc/syngo/env /etc/syngo/config.yaml" ./deploy/backup.sh
#
# Для docker compose - deploy/prod/backup-compose.sh backup (задает PSQL, PG_DUMP
# и MEDIA_TAR_CMD через docker).
#
# Единый снимок: открывается транзакция REPEATABLE READ и экспортируется ее
# снимок; pg_dump --snapshot, счетчики таблиц и список файлов медиа берутся из
# этого же снимка. Файлы медиа неизменяемы (запись во временный файл и rename,
# строка в БД - после файла), поэтому архив, снятый после снимка, содержит все
# файлы, на которые ссылается дамп. Если хотя бы одного файла нет в архиве,
# запуск завершается ошибкой до публикации, выноса и ротации копий.
#
# Результат: BACKUP_DIR/syngo-<stamp>/ (каталог 0700, файлы 0600):
#   db.dump           pg_dump --format=custom
#   media.tar.gz      файлы медиа (без временных .upload-*)
#   media-keys.tsv    storage_key и sha256 из снимка БД
#   media-missing.txt пустой список (совместимость с форматом syngo-backup/2)
#   counts.tsv        число строк каждой таблицы в снимке
#   manifest.txt      формат, время, снимок, версия схемы
#   secrets/          BACKUP_SECRET_FILES (ключ подписи, config.yaml, .env)
#   SHA256SUMS        суммы всех файлов
# С BACKUP_GPG_RECIPIENT - один файл syngo-<stamp>.tar.gpg вместо каталога.
#
# Переменные:
#   SYNGOD_DATABASE_DSN   база (обязательно)
#   MEDIA_DIR             каталог медиа (или MEDIA_TAR_CMD - команда, пишущая
#                         tar.gz каталога медиа в stdout)
#   BACKUP_DIR            куда класть (/var/backups/syngo)
#   KEEP                  сколько последних бэкапов хранить (7)
#   BACKUP_SECRET_FILES   файлы секретов и конфига через пробел
#   BACKUP_GPG_RECIPIENT  шифровать на этот ключ gpg (id или email)
#   BACKUP_UPLOAD_CMD     вынос копии: bash-команда, путь бэкапа - в "$1",
#                         например 'rsync -a -- "$1" backup@vault:/srv/syngo/'
#   BACKUP_METRICS_FILE   файл метрик для textfile collector node_exporter
#                         (/var/lib/node_exporter/textfile/syngo_backup.prom)
#   PSQL, PG_DUMP         команды psql и pg_dump (по умолчанию из PATH)
#
# RPO - интервал запуска (сутки по таймеру deploy/prod/syngo-backup.timer),
# RTO - время restore.sh (минуты на объемах до десятков ГБ).
set -euo pipefail
umask 077

BACKUP_DIR=${BACKUP_DIR:-/var/backups/syngo}
KEEP=${KEEP:-7}
MEDIA_DIR=${MEDIA_DIR:-}
MEDIA_TAR_CMD=${MEDIA_TAR_CMD:-}
DSN=${SYNGOD_DATABASE_DSN:-}
SECRET_FILES=${BACKUP_SECRET_FILES:-}
GPG_RECIPIENT=${BACKUP_GPG_RECIPIENT:-}
UPLOAD_CMD=${BACKUP_UPLOAD_CMD:-}
METRICS_FILE=${BACKUP_METRICS_FILE:-}
read -ra psql_cmd <<<"${PSQL:-psql}"
read -ra pg_dump_cmd <<<"${PG_DUMP:-pg_dump}"

started=$(date +%s)
stamp=$(date -u +%Y%m%dT%H%M%SZ)
work=""
snap_pid=""

die() {
	echo "ОШИБКА: $*" >&2
	exit 1
}

# write_metrics <success_ts> <failure_ts> - метрики для алертов SyngoBackup*.
# Значение, которое не меняется в этом запуске, берется из прежнего файла.
write_metrics() {
	[[ -n $METRICS_FILE ]] || return 0

	local success=$1 failure=$2 size=${3:-} duration=$(($(date +%s) - started)) prev

	prev_value() {
		[[ -f $METRICS_FILE ]] && awk -v m="$1" '$1 == m { print $2 }' "$METRICS_FILE" || true
	}

	if [[ -z $success ]]; then
		prev=$(prev_value syngo_backup_last_success_timestamp_seconds)
		success=${prev:-0}
	fi

	if [[ -z $failure ]]; then
		prev=$(prev_value syngo_backup_last_failure_timestamp_seconds)
		failure=${prev:-0}
	fi

	if [[ -z $size ]]; then
		prev=$(prev_value syngo_backup_last_size_bytes)
		size=${prev:-0}
	fi

	mkdir -p "$(dirname "$METRICS_FILE")"

	local tmp="$METRICS_FILE.$$"
	cat >"$tmp" <<-EOF
		# HELP syngo_backup_last_success_timestamp_seconds Время последнего успешного бэкапа (unix).
		# TYPE syngo_backup_last_success_timestamp_seconds gauge
		syngo_backup_last_success_timestamp_seconds $success
		# HELP syngo_backup_last_failure_timestamp_seconds Время последнего неудачного бэкапа (unix), 0 - не было.
		# TYPE syngo_backup_last_failure_timestamp_seconds gauge
		syngo_backup_last_failure_timestamp_seconds $failure
		# HELP syngo_backup_last_size_bytes Размер последнего успешного бэкапа.
		# TYPE syngo_backup_last_size_bytes gauge
		syngo_backup_last_size_bytes $size
		# HELP syngo_backup_last_duration_seconds Длительность последнего запуска.
		# TYPE syngo_backup_last_duration_seconds gauge
		syngo_backup_last_duration_seconds $duration
	EOF
	# node_exporter читает файл: права на чтение для него, атомарная замена.
	chmod 644 "$tmp"
	mv -f "$tmp" "$METRICS_FILE"
}

on_exit() {
	local rc=$?

	if [[ -n $snap_pid ]]; then
		kill "$snap_pid" 2>/dev/null || true
	fi

	if [[ $rc -ne 0 ]]; then
		[[ -n $work && -d $work ]] && rm -rf "$work"
		write_metrics "" "$(date +%s)" || true
		echo "FAIL: бэкап $stamp не выполнен (код $rc)" >&2
	fi
}

trap on_exit EXIT

[[ -n $DSN ]] || die "нужен SYNGOD_DATABASE_DSN"
[[ $KEEP =~ ^[1-9][0-9]*$ ]] || die "KEEP должен быть положительным числом"
command -v "${psql_cmd[0]}" >/dev/null || die "нет ${psql_cmd[0]}"
command -v "${pg_dump_cmd[0]}" >/dev/null || die "нет ${pg_dump_cmd[0]}"
command -v sha256sum >/dev/null || die "нет sha256sum"

if [[ -n $GPG_RECIPIENT ]]; then
	command -v gpg >/dev/null || die "BACKUP_GPG_RECIPIENT задан, а gpg нет"
fi

if [[ -z $MEDIA_TAR_CMD && -n $MEDIA_DIR && ! -d $MEDIA_DIR ]]; then
	die "каталог медиа $MEDIA_DIR не найден (MEDIA_DIR)"
fi

for file in $SECRET_FILES; do
	[[ -f $file && -r $file ]] || die "нет файла секретов $file (BACKUP_SECRET_FILES)"
done

mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"

# Один бэкап за раз: второй запуск таймера при долгом первом - отказ.
if command -v flock >/dev/null; then
	exec 9>"$BACKUP_DIR/.lock"
	flock -n 9 || die "бэкап уже выполняется ($BACKUP_DIR/.lock)"
fi

# Остатки прерванных запусков (lock взят - параллельных нет).
find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -name '.tmp-*' -exec rm -rf {} +

work="$BACKUP_DIR/.tmp-$stamp"
mkdir "$work"

echo "==> снимок БД"
# Транзакция со снимком держится в фоновом psql, пока идут дамп и счетчики.
coproc SNAP { "${psql_cmd[@]}" -X -q -A -t -v ON_ERROR_STOP=1 "$DSN" 2>&1; }
# shellcheck disable=SC2153 # SNAP_PID задает coproc
snap_pid=$SNAP_PID
printf '%s\n' "SET idle_in_transaction_session_timeout = 0;" \
	"BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;" "SELECT pg_export_snapshot();" >&"${SNAP[1]}"
IFS= read -r -t 60 snapshot <&"${SNAP[0]}" || die "не получен снимок БД"
[[ $snapshot =~ ^[0-9A-F-]+$ ]] || die "не получен снимок БД: $snapshot"

# Запрос в том же снимке: отдельная транзакция с SET TRANSACTION SNAPSHOT.
in_snapshot() {
	{
		printf '%s\n' "BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;" "SET TRANSACTION SNAPSHOT '$snapshot';"
		cat
		printf '%s\n' "COMMIT;"
	} | "${psql_cmd[@]}" -X -q -A -t -F $'\t' -v ON_ERROR_STOP=1 "$DSN"
}

echo "==> дамп БД (снимок $snapshot)"
"${pg_dump_cmd[@]}" --format=custom --no-owner --no-privileges --snapshot="$snapshot" "$DSN" >"$work/db.dump"

echo "==> счетчики таблиц"
in_snapshot >"$work/counts.tsv" <<'SQL'
SELECT format('SELECT %L, count(*) FROM %I.%I', tablename, schemaname, tablename)
FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename \gexec
SQL

schema=$(in_snapshot <<<"SELECT version || ' ' || dirty FROM schema_migrations;")
in_snapshot >"$work/media-keys.tsv" <<'SQL'
SELECT storage_key, coalesce(encode(sha256, 'hex'), '') FROM media WHERE storage_key <> '' ORDER BY storage_key;
SQL

# Снимок больше не нужен.
printf '%s\n' "COMMIT;" '\q' >&"${SNAP[1]}"
wait "$snap_pid" || true
snap_pid=""

media="absent"
: >"$work/media-missing.txt"

if [[ -n $MEDIA_TAR_CMD || -n $MEDIA_DIR ]]; then
	echo "==> архив медиа"

	if [[ -n $MEDIA_TAR_CMD ]]; then
		bash -c "$MEDIA_TAR_CMD" >"$work/media.tar.gz"
	else
		tar -czf "$work/media.tar.gz" -C "$MEDIA_DIR" --exclude='.upload-*' .
	fi

	media="included"

	# Файлы снимка, которых нет в архиве (удалены за время архивации).
	tar -tzf "$work/media.tar.gz" | sed 's#^\./##' | sort >"$work/.archived"
	cut -f1 "$work/media-keys.tsv" | sort -u | comm -23 - "$work/.archived" >"$work/media-missing.txt"
	rm -f "$work/.archived"

	if [[ -s $work/media-missing.txt ]]; then
		die "в архиве нет файлов из снимка БД: $(wc -l <"$work/media-missing.txt"); первый: $(head -n1 "$work/media-missing.txt")"
	fi
elif [[ -s $work/media-keys.tsv ]]; then
	die "в снимке БД есть медиа: задайте MEDIA_DIR или MEDIA_TAR_CMD"
fi

if [[ -n $SECRET_FILES ]]; then
	echo "==> секреты и конфиг"
	mkdir "$work/secrets"

	for file in $SECRET_FILES; do
		cp -- "$file" "$work/secrets/$(basename "$file")"
	done
fi

secrets=""
if [[ -d $work/secrets ]]; then
	secrets=$(cd "$work" && find secrets -type f | sort | tr '\n' ' ')
fi

cat >"$work/manifest.txt" <<EOF
format=syngo-backup/2
stamp=$stamp
created_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
snapshot=$snapshot
schema_version=${schema% *}
schema_dirty=${schema#* }
media=$media
media_files=$(wc -l <"$work/media-keys.tsv")
media_missing=$(wc -l <"$work/media-missing.txt")
secrets=${secrets% }
EOF

echo "==> контрольные суммы"
(cd "$work" && find . -type f | sed 's#^\./##' | sort | xargs -d '\n' sha256sum) >"$work.sums"
mv "$work.sums" "$work/SHA256SUMS"
chmod -R go-rwx "$work"
find "$work" -type f -exec chmod 600 {} +

final="$BACKUP_DIR/syngo-$stamp"
mv "$work" "$final"
work=""
artifact=$final

if [[ -n $GPG_RECIPIENT ]]; then
	echo "==> шифрование на $GPG_RECIPIENT"
	work="$final"
	tar -cf - -C "$BACKUP_DIR" "syngo-$stamp" |
		gpg --batch --yes --trust-model always --encrypt --recipient "$GPG_RECIPIENT" \
			--output "$final.tar.gpg.tmp"
	mv "$final.tar.gpg.tmp" "$final.tar.gpg"
	chmod 600 "$final.tar.gpg"
	rm -rf "$final"
	work=""
	artifact="$final.tar.gpg"
fi

if [[ -n $UPLOAD_CMD ]]; then
	echo "==> вынос копии"
	bash -c "$UPLOAD_CMD" upload "$artifact"
fi

# Ротация: KEEP последних (имена сортируются по времени), текущий не трогаем.
mapfile -t backups < <(find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -name 'syngo-*' \
	\( -type d -o -name 'syngo-*.tar.gpg' \) | sort -r)

for ((i = KEEP; i < ${#backups[@]}; i++)); do
	[[ ${backups[$i]} == "$artifact" ]] && continue
	echo "==> ротация: удаляю $(basename "${backups[$i]}")"
	rm -rf -- "${backups[$i]}"
done

size=$(du -sb "$artifact" | cut -f1)
write_metrics "$(date +%s)" "" "$size"

echo "OK: бэкап $artifact (схема ${schema% *}, медиа: $media, храним $KEEP)"
