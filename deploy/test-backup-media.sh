#!/usr/bin/env bash
# Проверка backup, verify и restore на PostgreSQL. Параметры сервера задаются
# через PGHOST, PGPORT, PGUSER, PGPASSWORD. Роль должна уметь создавать базы.
set -euo pipefail

deploy=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
source_db="syngo_backup_test_$$_$RANDOM"
target_db="syngo_restore_test_$$_$RANDOM"
databases=()
cleanup() {
	for db in "${databases[@]}"; do dropdb --maintenance-db=postgres "$db"; done
	rm -rf "$work"
}
trap cleanup EXIT
for db in "$source_db" "$target_db"; do
	createdb --maintenance-db=postgres "$db"
	databases+=("$db")
done

fail() { echo "FAIL: $*" >&2; exit 1; }
export SYNGOD_DATABASE_DSN="postgresql:///$source_db" MEDIA_DIR="$work/media" BACKUP_DIR="$work/backups"
export BACKUP_METRICS_FILE="$work/backup.prom" KEEP=1
target_dsn="postgresql:///$target_db"
mkdir -p "$MEDIA_DIR/local"
printf 'media from snapshot\n' >"$MEDIA_DIR/local/file"
hash=$(sha256sum "$MEDIA_DIR/local/file" | cut -d' ' -f1)
psql -Xq -v ON_ERROR_STOP=1 -d "$source_db" -v hash="$hash" <<'SQL'
CREATE TABLE schema_migrations (version bigint, dirty boolean);
INSERT INTO schema_migrations VALUES (49, false);
CREATE TABLE media (storage_key text, sha256 bytea);
INSERT INTO media VALUES ('local/file', decode(:'hash', 'hex'));
SQL

"$deploy/backup.sh"
copies=("$BACKUP_DIR"/syngo-*)
copy=${copies[0]}
"$deploy/verify-restore.sh" "$copy"
SYNGOD_DATABASE_DSN=$target_dsn MEDIA_DIR=$work/restored "$deploy/restore.sh" "$copy"
cmp "$MEDIA_DIR/local/file" "$work/restored/local/file"
rows="SELECT storage_key, encode(sha256, 'hex') FROM media ORDER BY storage_key"
source_rows=$(psql -XAt -v ON_ERROR_STOP=1 -d "$source_db" -c "$rows")
target_rows=$(psql -XAt -v ON_ERROR_STOP=1 -d "$target_db" -c "$rows")
[[ $source_rows == "local/file|$hash" && $target_rows == "$source_rows" ]] || fail 'метаданные медиа различаются'
success=$(awk '$1 == "syngo_backup_last_success_timestamp_seconds" {print $2}' "$BACKUP_METRICS_FILE")
((success > 0)) || fail 'нет метрики успешной копии'

rm "$MEDIA_DIR/local/file"
if "$deploy/backup.sh"; then fail 'backup принял архив без файла'; fi
grep -qx "syngo_backup_last_success_timestamp_seconds $success" "$BACKUP_METRICS_FILE" || fail 'изменилась метрика успешной копии'
grep -Eq '^syngo_backup_last_failure_timestamp_seconds [1-9][0-9]*$' "$BACKUP_METRICS_FILE" || fail 'нет метрики ошибки'
after=("$BACKUP_DIR"/syngo-*)
[[ ${#after[@]} == 1 && ${after[0]} == "$copy" ]] || fail 'изменился набор опубликованных копий'
"$deploy/verify-restore.sh" "$copy"
if MEDIA_DIR='' MEDIA_TAR_CMD='' "$deploy/backup.sh"; then fail 'backup принял снимок с медиа без источника файлов'; fi

# Старая копия v2 могла иметь верные SHA256SUMS и явно перечислять потерянный файл.
legacy=$work/legacy
cp -a "$copy" "$legacy"
tar -czf "$legacy/media.tar.gz" -C "$MEDIA_DIR" .
printf 'local/file\n' >"$legacy/media-missing.txt"
sed -i 's/^media_missing=.*/media_missing=1/' "$legacy/manifest.txt"
checksums() {
	(cd "$legacy" && find . -type f ! -name SHA256SUMS -exec sha256sum {} +) >"$work/sums"
	mv "$work/sums" "$legacy/SHA256SUMS"
}
checksums
if "$deploy/verify-restore.sh" "$legacy"; then fail 'verify принял старую неполную копию'; fi
psql -Xq -v ON_ERROR_STOP=1 -d "$target_db" -c "UPDATE media SET storage_key='target-marker'"
if RESTORE_FORCE=1 SYNGOD_DATABASE_DSN=$target_dsn MEDIA_DIR=$work/restored "$deploy/restore.sh" "$legacy"; then
	fail 'restore принял старую неполную копию'
fi
[[ $(psql -XAt -d "$target_db" -c 'SELECT storage_key FROM media') == target-marker ]] || fail 'restore изменил целевую базу'
[[ $(sha256sum "$work/restored/local/file" | cut -d' ' -f1) == "$hash" ]] || fail 'restore изменил целевые медиа'

rm "$legacy/media.tar.gz"
sed -i 's/^media=included$/media=absent/' "$legacy/manifest.txt"
checksums
if "$deploy/verify-restore.sh" "$legacy"; then fail 'verify принял копию без архива при наличии ссылок'; fi
psql -Xq -v ON_ERROR_STOP=1 -d "$source_db" -c 'DELETE FROM media'
MEDIA_DIR='' MEDIA_TAR_CMD='' BACKUP_DIR=$work/db-only "$deploy/backup.sh"
empty_copy=("$work/db-only"/syngo-*)
"$deploy/verify-restore.sh" "${empty_copy[0]}"
echo 'OK: полная копия восстанавливается, неполная отклоняется до замены данных'
