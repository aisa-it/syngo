#!/bin/sh
# Entrypoint образа для Complement: поднимает локальный PostgreSQL, накатывает миграции
# и запускает syngod. Complement передаёт только SERVER_NAME — остальное готовим сами,
# потому что sidecar-сервисы он не умеет.
set -eu

: "${SERVER_NAME:?SERVER_NAME не задан: Complement обязан передать имя сервера}"

export SYNGOD_MATRIX_SERVER_NAME="${SYNGOD_MATRIX_SERVER_NAME:-$SERVER_NAME}"
# Complement-образ всегда тестовый: без этого syngod в prod-режиме отказывается стартовать
# без sync.token_secret (валидация конфига).
export SYNGOD_ENV="${SYNGOD_ENV:-dev}"
export SYNGOD_SYNC_TOKEN_SECRET="${SYNGOD_SYNC_TOKEN_SECRET:-complement-test-secret}"
export SYNGOD_SERVER_ADDR="${SYNGOD_SERVER_ADDR:-:8008}"
# Complement запускает тесты параллельно: rc-лимиты регистрации/логина давали 429 и
# валили TestRegistration. В тестовом контуре лимиты выключаем.
export SYNGOD_RATE_LIMIT_DISABLED="${SYNGOD_RATE_LIMIT_DISABLED:-1}"
# Регистрация по умолчанию закрыта, а Complement создает пользователей
# через /register и /_synapse/admin/v1/register (общий секрет "complement").
export SYNGOD_AUTH_REGISTRATION="${SYNGOD_AUTH_REGISTRATION:-true}"
export SYNGOD_REGISTRATION_SHARED_SECRET="${SYNGOD_REGISTRATION_SHARED_SECRET:-complement}"

PGDATA=/tmp/pgdata
PGSOCK=/tmp/pgsock
PGPORT=5432
mkdir -p "$PGDATA" "$PGSOCK"
# Каталоги создаются пустыми при каждом старте: рекурсивный chown не нужен, а на
# заполненном PGDATA он копировал бы все файлы слоя. Меняем владельца,
# только если он не postgres.
for dir in "$PGDATA" "$PGSOCK"; do
    [ "$(stat -c %U "$dir")" = postgres ] || chown postgres:postgres "$dir"
done

# Старт контейнера на загруженном раннере занимал около 60 с при таймауте Complement
# 30 с: основное время - fsync в initdb и миграциях. База
# одноразовая, поэтому fsync выключен и в initdb, и в сервере.
if [ ! -f "$PGDATA/PG_VERSION" ]; then
    echo "complement: initdb"
    su postgres -c "initdb -D $PGDATA -A trust -U postgres --no-sync" >/dev/null
else
    # PGDATA пришел из снимка blueprint (docker commit работающего контейнера).
    # Recovery трогает почти все файлы кластера, и каждый первый write в файл нижнего
    # слоя overlayfs - это copy-up с fsync: на занятом диске recovery шел минутами,
    # Complement не дожидался старта. Копия - новые файлы верхнего слоя,
    # без copy-up и fsync.
    echo "complement: PGDATA из снимка, копирую в верхний слой"
    rm -rf "$PGDATA.copy"
    cp -a "$PGDATA" "$PGDATA.copy"
    rm -rf "$PGDATA"
    mv "$PGDATA.copy" "$PGDATA"
    rm -f "$PGDATA/postmaster.pid"
fi

echo "complement: запускаю PostgreSQL на порту $PGPORT"
su postgres -c "pg_ctl -D $PGDATA -l /tmp/pg.log -o '-p $PGPORT -k $PGSOCK -c listen_addresses=127.0.0.1 -c fsync=off -c synchronous_commit=off -c full_page_writes=off' start"

export SYNGOD_DATABASE_DSN="${SYNGOD_DATABASE_DSN:-postgres://postgres@127.0.0.1:$PGPORT/postgres?sslmode=disable}"

attempt=0
until psql "$SYNGOD_DATABASE_DSN" -tAc 'select 1' >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if [ "$attempt" -ge 30 ]; then
        echo "complement: PostgreSQL не поднялся" >&2
        cat /tmp/pg.log >&2 || true
        exit 1
    fi

    sleep 1
done

syngo-migrate -dsn "$SYNGOD_DATABASE_DSN" -path /opt/syngo/migrations -up

echo "complement: стартую syngod как ${SYNGOD_MATRIX_SERVER_NAME}"
exec syngod
