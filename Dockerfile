# Multi-stage сборка. Последняя стадия - prod (distroless, nonroot): `docker build .`
# без --target собирает именно продовый образ. Стадии complement и stand
# собираются только явно (--target complement / --target stand).

FROM golang:1.27.4-alpine AS build

WORKDIR /src

# Слой зависимостей кэшируется отдельно от исходников.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

ENV CGO_ENABLED=0 GOOS=linux
# syngod и syngo-migrate: миграции встроены в оба бинаря, каталог
# migrations в образ не нужен.
RUN LDFLAGS="-s -w \
        -X github.com/aisa-it/syngo/internal/build.Version=${VERSION} \
        -X github.com/aisa-it/syngo/internal/build.Commit=${COMMIT} \
        -X github.com/aisa-it/syngo/internal/build.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    && go build -trimpath -ldflags "$LDFLAGS" -o /out/syngod ./cmd/syngod \
    && go build -trimpath -ldflags "$LDFLAGS" -o /out/syngo-migrate ./cmd/migrate \
    && mkdir -p /out/var/syngo/media

# --- Образ для Complement -------------------------------------------------
# Complement запускает homeserver как контейнер и передаёт SERVER_NAME в окружении.
# Наш сервер требует PostgreSQL, а Complement не поднимает sidecar-сервисы, поэтому
# образ самодостаточный: внутри контейнера стартует PostgreSQL (postgres:17-alpine),
# накатываются миграции и запускается syngod на 8008. Продовый образ остаётся distroless.
FROM golang:1.27.4-alpine AS complement-build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -o /out/syngod ./cmd/syngod \
    && go build -trimpath -o /out/syngo-migrate ./cmd/migrate

FROM postgres:17-alpine AS complement

COPY --from=complement-build /out/syngod /usr/local/bin/syngod
COPY --from=complement-build /out/syngo-migrate /usr/local/bin/syngo-migrate
COPY migrations /opt/syngo/migrations
COPY LICENSE NOTICE /licenses/
COPY tools/complement/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh

EXPOSE 8008 8448

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

# --- Образ для локального стенда федерации --------------------------------
# В отличие от продового distroless здесь нужны shell/curl для healthcheck, миграции
# и утилиты генерации ключей: стенд запускается одной командой make stand-up.
FROM golang:1.27.4-alpine AS stand-build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -o /out/syngod ./cmd/syngod \
    && go build -trimpath -o /out/syngo-migrate ./cmd/migrate \
    && go build -trimpath -o /out/syngo-keygen ./cmd/federation-keygen \
    && go build -trimpath -o /out/syngo-interop ./cmd/syngo-interop

FROM alpine:3.20 AS stand

RUN adduser -D -u 10001 syngo \
    && mkdir -p /var/syngo/media \
    && chown -R syngo:syngo /var/syngo

COPY --from=stand-build /out/syngod /usr/local/bin/syngod
COPY --from=stand-build /out/syngo-migrate /usr/local/bin/syngo-migrate
COPY --from=stand-build /out/syngo-keygen /usr/local/bin/syngo-keygen
COPY --from=stand-build /out/syngo-interop /usr/local/bin/syngo-interop
COPY migrations /opt/syngo/migrations
COPY LICENSE NOTICE /licenses/

USER syngo
EXPOSE 8448

ENTRYPOINT ["/usr/local/bin/syngod"]

# --- Продовый образ -----------------------------------------
# distroless без shell и пакетного менеджера: меньше поверхность атаки. Стадия
# последняя, чтобы `docker build .` собирал ее по умолчанию.
FROM gcr.io/distroless/static-debian12:nonroot AS prod

COPY --from=build /out/syngod /syngod
COPY --from=build /out/syngo-migrate /syngo-migrate
COPY LICENSE NOTICE /licenses/
# Каталог медиа принадлежит nonroot (65532): пустой именованный том docker
# инициализирует из образа с теми же правами, upload не падает с 500.
COPY --from=build --chown=65532:65532 /out/var/syngo /var/syngo

ENV SYNGOD_SERVER_ADDR=:8008 \
    SYNGOD_MEDIA_DIRECTORY=/var/syngo/media

USER nonroot:nonroot
EXPOSE 8008 8448

# curl в образе нет: проверку /health/ready делает сам бинарь.
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=5 CMD ["/syngod", "-healthcheck"]

ENTRYPOINT ["/syngod"]
