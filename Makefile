# Makefile synapse-go. Цели описаны в README.
MODULE  ?= github.com/aisa-it/syngo
BIN     ?= bin/syngod
PKG     ?= ./...

# Проект на Go 1.27: инструменты (golangci-lint, govulncheck, …) должны собираться
# тем же тулчейном, иначе линтер отказывается анализировать проект, а govulncheck
# не понимает свежий stdlib. GOTOOLCHAIN заставляет go использовать
# ровно 1.27.4 (патченный stdlib) даже на машинах с базовым go 1.24.
GOTOOLCHAIN ?= go1.27.4
export GOTOOLCHAIN

# Версии инструментов фиксируем: @latest у golangci-lint тянул свой go-directive.
GOLANGCI_VERSION     ?= v2.15.0
# v1.10.0 падает на stdlib go1.27 («generic method requires go1.27»): x/tools отстаёт.
# v1.9.0 сканирует go1.27 корректно, поэтому пин на неё.
GOVULNCHECK_VERSION  ?= v1.9.0
STATICCHECK_VERSION  ?= latest
GOFUMPT_VERSION      ?= latest
GOIMPORTS_VERSION    ?= latest
GOBIN   := $(shell go env GOPATH)/bin
export PATH := $(PATH):$(GOBIN)

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/build.Version=$(VERSION) \
	-X $(MODULE)/internal/build.Commit=$(COMMIT) \
	-X $(MODULE)/internal/build.BuildTime=$(BUILT)

.PHONY: help
help: ## список целей
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: build
build: ## собрать бинарь в bin/syngod
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/syngod

.PHONY: run
run: ## запустить сервис локально (dev, server_name localhost, регистрация открыта, секрет токенов случайный)
	SYNGOD_ENV=$${SYNGOD_ENV:-dev} \
	SYNGOD_MATRIX_SERVER_NAME=$${SYNGOD_MATRIX_SERVER_NAME:-localhost} \
	SYNGOD_AUTH_REGISTRATION=$${SYNGOD_AUTH_REGISTRATION:-true} \
	go run ./cmd/syngod

.PHONY: fmt
fmt: ## gofumpt + goimports
	gofumpt -l -w .
	goimports -local $(MODULE) -w .

.PHONY: vet
vet: ## go vet
	go vet $(PKG)

.PHONY: lint
lint: ## golangci-lint (конфиг .golangci.yml, v2)
	golangci-lint run $(PKG)

.PHONY: test
test: ## тесты без race (быстрая итерация)
	go test -shuffle=on $(PKG)

.PHONY: test-race
test-race: ## тесты с -race (как в CI)
	go test -race -shuffle=on -count=1 $(PKG)

.PHONY: cover
cover: ## покрытие в cover.out
	go test -race -covermode=atomic -coverprofile=cover.out $(PKG)
	go tool cover -func=cover.out | tail -1

COVERAGE_BASELINE ?= test/coverage-baseline.txt

.PHONY: perf
perf: ## perf-ratchet /sync на профиле bench/sync-baseline.json (нужен PSQL_DSN бенч-базы)
	bash scripts/perf-ratchet.sh

.PHONY: perf-baseline
perf-baseline: ## снять baseline /sync на этом железе (ROOMS/EVENTS/CLIENTS - профиль)
	bash scripts/perf-ratchet.sh --update

.PHONY: coverage
coverage: ## тесты с -race и покрытием + ratchet по пакетам (test/coverage-baseline.txt)
	COVERAGE_PROFILE=$(CURDIR)/cover.out bash scripts/coverage-ratchet.sh

.PHONY: coverage-baseline
coverage-baseline: ## поднять baseline покрытия (понижение - только правкой файла в PR)
	COVERAGE_PROFILE=$(CURDIR)/cover.out bash scripts/coverage-ratchet.sh --update

.PHONY: gates-test
gates-test: ## тесты ratchet-гейтов на ухудшенных данных (coverage, perf, Complement; без Docker)
	python3 -m unittest discover -s tools/ratchet -p 'test_*.py'
	python3 -m unittest discover -s tools/complement -p 'test_*.py'

.PHONY: vuln
vuln: ## проверка уязвимостей (govulncheck v1.9.0, go1.27)
	govulncheck $(PKG)

.PHONY: check
check: fmt vet coverage gates-test lint vuln ## полный локальный прогон перед PR (coverage = test-race + ratchet)

.PHONY: migrate
migrate: ## применить миграции (golang-migrate; нужен PSQL_DSN)
	@if [ -z "$(PSQL_DSN)" ]; then echo "PSQL_DSN is required"; exit 1; fi
	go run ./cmd/migrate -dsn "$(PSQL_DSN)" -up

.PHONY: migrate-down
migrate-down: ## откатить N миграций (PSQL_DSN, N обязателен; на базе без test в имени - YES=1)
	@if [ -z "$(PSQL_DSN)" ]; then echo "PSQL_DSN is required"; exit 1; fi
	@if [ -z "$(N)" ]; then echo "N is required: make migrate-down N=1 (откат всех миграций одной командой запрещен)"; exit 1; fi
	go run ./cmd/migrate -dsn "$(PSQL_DSN)" -down $(N) $(if $(filter 1 yes true,$(YES)),-yes)

.PHONY: migrate-version
migrate-version: ## текущая версия схемы
	@if [ -z "$(PSQL_DSN)" ]; then echo "PSQL_DSN is required"; exit 1; fi
	go run ./cmd/migrate -dsn "$(PSQL_DSN)" -version

## Локальный стенд федерации: deploy/stand.
STAND_DIR := deploy/stand

.PHONY: backup
backup: ## бэкап БД и медиа (deploy/backup.sh)
	./deploy/backup.sh

.PHONY: restore
restore: ## восстановление из бэкапа: make restore DUMP=... [MEDIA=...]
	./deploy/restore.sh $(DUMP) $(MEDIA)

.PHONY: verify-restore
verify-restore: ## проверка бэкапа на чистой БД
	./deploy/verify-restore.sh $(DUMP) $(MEDIA)

.PHONY: stand-up
stand-up: ## поднять стенд федерации (bootstrap: ключи, БД, пользователи, комната)
	$(STAND_DIR)/bootstrap.sh

.PHONY: stand-smoke
stand-smoke: ## smoke-сценарий федерации (инвайт, join, порядок, leave, рестарт, идемпотентность)
	$(STAND_DIR)/smoke.sh

.PHONY: stand-interop
stand-interop: ## интероп-прогон с живым Synapse: сценарии + сверка room_state
	$(STAND_DIR)/interop.sh

.PHONY: stand-interop-keep
stand-interop-keep: ## интероп-прогон без остановки стенда (разбор расхождений)
	$(STAND_DIR)/interop.sh --keep

.PHONY: stand-down
stand-down: ## остановить стенд (данные и ключи сохраняются)
	cd $(STAND_DIR) && docker compose down

.PHONY: stand-logs
stand-logs: ## логи стенда (все сервисы, follow)
	cd $(STAND_DIR) && docker compose logs -f --tail=100

.PHONY: stand-reset
stand-reset: ## снести стенд вместе с томами, ключами и состоянием
	cd $(STAND_DIR) && docker compose -f docker-compose.yml -f docker-compose.synapse.yml down -v --remove-orphans
	rm -rf $(STAND_DIR)/keys/*.key $(STAND_DIR)/.stand-state.env $(STAND_DIR)/.env

INTEGRATION_GROUP ?= all
INTEGRATION_TEST_FLAGS ?= -shuffle=on
integration_api := ./internal/api/
integration_storage_import := ./internal/store/ ./internal/importevents/ ./internal/exportdata/ ./cmd/migrate/ ./cmd/syngod/ ./cmd/syngo-import/ ./cmd/syngo-import-media/
integration_all := $(integration_api) $(integration_storage_import)

.PHONY: test-integration
test-integration: ## интеграционные тесты (нужен SYNGOD_TEST_DSN на живую БД)
	@if [ -z "$(SYNGOD_TEST_DSN)" ]; then echo "SYNGOD_TEST_DSN is required"; exit 1; fi
	@test -n "$(integration_$(INTEGRATION_GROUP))" || { echo "Unknown INTEGRATION_GROUP: $(INTEGRATION_GROUP)" >&2; exit 1; }
	# -p 1: пакеты не должны параллельно дропать общую тестовую схему
	go test -tags integration -count=1 -p 1 -timeout 30m $(INTEGRATION_TEST_FLAGS) $(integration_$(INTEGRATION_GROUP))

.PHONY: test-complement
test-complement: ## прогнать Complement и проверить ratchet (нужен Docker)
	bash scripts/complement.sh

.PHONY: complement-ratchet-test
complement-ratchet-test: ## тесты нормализации и ratchet (без Docker)
	python3 -m unittest discover -s tools/complement -p 'test_*.py'

.PHONY: complement-baseline
complement-baseline: ## обновить test/complement/results.jsonl по текущему прогону (нужен Docker)
	bash scripts/complement.sh --update-baseline

.PHONY: bench-sync
bench-sync: ## нагрузочный стенд /sync (PSQL_DSN бенч-базы: имя с test/bench/perf)
	@if [ -z "$(PSQL_DSN)" ]; then echo "PSQL_DSN is required (отдельная бенч-база, не SYNGOD_DATABASE_DSN)"; exit 1; fi
	go run ./cmd/syncload -dsn "$(PSQL_DSN)" -scenario $${SCENARIO:-all} \
		-rooms $${ROOMS:-1000} -events-per-room $${EVENTS:-20} -clients $${CLIENTS:-10} -out-dir bench

## Нагрузочное тестирование сервера по сети (docs/load-testing.md).
PROFILE ?= S
LOAD_ARGS ?=

.PHONY: load-stand-up
load-stand-up: ## поднять нагрузочный стенд syngod (порт 38008, ops 39108, БД LOAD_DSN; LOAD_RATE_LIMIT=prod|off)
	bash scripts/load-stand.sh up

.PHONY: load-stand-down
load-stand-down: ## остановить нагрузочный стенд
	bash scripts/load-stand.sh down

.PHONY: load-stand-reset
load-stand-reset: ## очистить БД и медиа стенда (остановленного)
	bash scripts/load-stand.sh reset

.PHONY: load-smoke
load-smoke: ## smoke: поднять стенд, профиль S (~2 мин), остановить стенд, если его поднимал этот запуск
	@started=0; bash scripts/load-stand.sh status >/dev/null 2>&1 || { bash scripts/load-stand.sh up && started=1; }; \
	bash scripts/load-run.sh syngo S $(LOAD_ARGS); rc=$$?; \
	if [ $$started = 1 ]; then bash scripts/load-stand.sh down; fi; exit $$rc

.PHONY: load
load: ## прогон профиля PROFILE (S|M|L|файл.json) на поднятом стенде: make load PROFILE=M LOAD_ARGS="-duration 5m"
	bash scripts/load-run.sh syngo $(PROFILE) $(LOAD_ARGS)

.PHONY: load-synapse-up
load-synapse-up: ## поднять Synapse для сравнения (docker, порт 38018, метрики 39118)
	bash scripts/load-synapse.sh up

.PHONY: load-synapse-down
load-synapse-down: ## остановить Synapse сравнения (тома сохраняются; reset - scripts/load-synapse.sh reset)
	bash scripts/load-synapse.sh down

.PHONY: load-synapse
load-synapse: ## тот же прогон против Synapse: make load-synapse PROFILE=S
	bash scripts/load-run.sh synapse $(PROFILE) $(LOAD_ARGS)

.PHONY: load-test
load-test: ## юнит-тесты генератора нагрузки (без сервера)
	go test -race -count=1 ./cmd/loadgen/ ./internal/benchenv/

.PHONY: export
export: ## выгрузить данные в JSONL (PSQL_DSN; OUT=syngo-dump.jsonl, MEDIA_DIR/MEDIA_OUT — опционально)
	@if [ -z "$(PSQL_DSN)" ]; then echo "PSQL_DSN is required"; exit 1; fi
	go run ./cmd/syngo-export -dsn "$(PSQL_DSN)" -out "$(or $(OUT),syngo-dump.jsonl)" \
		-media-dir "$(MEDIA_DIR)" -media-out "$(MEDIA_OUT)" -report "$(or $(REPORT),export-report.txt)"

.PHONY: docker
docker: ## собрать образ syngo:dev
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t syngo:dev .

.PHONY: tools
tools: ## поставить инструменты разработки (проектным тулчейном Go 1.27)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)
	go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	go install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)

.PHONY: clean
clean: ## удалить артефакты сборки
	rm -rf bin cover.out
