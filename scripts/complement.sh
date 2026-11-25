#!/usr/bin/env bash
# Complement против нашего homeserver.
#
# Что делает:
#   1) собирает тестовый образ syngo:complement (Dockerfile, target complement);
#   2) берёт Complement, зафиксированный на конкретном коммите (COMPLEMENT_REF) в /tmp;
#   3) гоняет `go test -json` по нужным пакетам и складывает сырые события в файл;
#   4) нормализует результаты и проверяет ratchet против test/complement/results.jsonl.
#
# Требуется Docker и Go 1.27+. Обновить baseline (только если регрессий нет и набор
# тестов не сократился):
#   scripts/complement.sh --update-baseline
#
# Взять baseline из артефакта CI (без Docker, через ту же проверку ratchet):
#   scripts/complement.sh --adopt-results=complement-results.jsonl
set -euo pipefail

cd "$(dirname "$0")/.."

IMAGE="${COMPLEMENT_IMAGE:-syngo:complement}"
COMPLEMENT_REF="${COMPLEMENT_REF:-50511896440c2f1ceb627c6f6f46c4a313ee0955}"  # пин на коммит: тегов у Complement нет
PACKAGES="${COMPLEMENT_PACKAGES:-./tests/csapi/... ./tests/...}"
RESULTS_RAW="${RESULTS_RAW:-/tmp/complement-go-test.jsonl}"
RESULTS_LOG="${RESULTS_LOG:-/tmp/complement-go-test.log}"
# Нормализованный текущий прогон (артефакт CI, вход для --adopt-results).
RESULTS_NORMALIZED="${RESULTS_NORMALIZED:-/tmp/complement-results.jsonl}"
WORKDIR="${COMPLEMENT_WORKDIR:-/tmp/complement}"
# Сборка Complement на 6 GiB/4 vCPU падала по OOM: ограничиваем параллелизм компиляции.
GOFLAGS_PARALLEL="${COMPLEMENT_GOFLAGS:--p=2}"
# Старт контейнера (initdb + миграции) на загруженном раннере дольше штатных 30 с
# Complement: все тесты падали на «Failed to deploy image».
export COMPLEMENT_SPAWN_HS_TIMEOUT_SECS="${COMPLEMENT_SPAWN_HS_TIMEOUT_SECS:-120}"
# Один homeserver на пакет вместо контейнера из снимка на каждый тест - так Complement
# гоняет Synapse (scripts-dev/complement.sh). Без этого csapi на занятом хосте не
# укладывался в таймаут: сотни стартов контейнера по минуте.
export COMPLEMENT_ENABLE_DIRTY_RUNS="${COMPLEMENT_ENABLE_DIRTY_RUNS:-1}"
# Общий таймаут go test: 20 минут не хватало; 35 минут + сборка укладываются в
# timeout джобы complement в CI (45 минут).
GO_TEST_TIMEOUT="${COMPLEMENT_GO_TEST_TIMEOUT:-35m}"

update_baseline=0
adopt_results=""
for arg in "$@"; do
    case "$arg" in
        --update-baseline) update_baseline=1 ;;
        # Взять уже нормализованные результаты из артефакта CI (test/complement/results.jsonl)
        # и сделать их baseline без Docker.
        --adopt-results=*) adopt_results="${arg#*=}" ;;
        -h|--help)
            sed -n '2,15p' "$0"

            exit 0
            ;;
        *) echo "неизвестный аргумент: $arg" >&2; exit 2 ;;
    esac
done

# Режим «взять готовые результаты из артефакта»: Docker не нужен, нужен только файл.
# Раньше файл копировался поверх baseline при одном pass - ratchet обходился;
# теперь он проходит ту же проверку, что и прогон.
if [ -n "$adopt_results" ]; then
    [ -f "$adopt_results" ] || { echo "нет файла: $adopt_results" >&2; exit 1; }

    exec python3 tools/complement/complement_ratchet.py check --input "$adopt_results" --update --print-pass-rate
fi

command -v docker >/dev/null || { echo "нужен docker" >&2; exit 1; }

echo "== сборка образа $IMAGE (target complement)"
docker build --target complement -t "$IMAGE" .

if [ ! -d "$WORKDIR/.git" ]; then
    echo "== клон Complement $COMPLEMENT_REF в $WORKDIR"
    rm -rf "$WORKDIR"
    # Complement не публикует теги, поэтому пин — коммит: клонируем и переключаемся.
    git clone https://github.com/matrix-org/complement "$WORKDIR"
    git -C "$WORKDIR" checkout --detach "$COMPLEMENT_REF"
fi

echo "== окружение: $(nproc 2>/dev/null || echo '?') CPU, $(free -g 2>/dev/null | awk '/Mem:/{print $2}' || echo '?') GiB RAM, GOFLAGS=$GOFLAGS_PARALLEL"
echo "== прогон Complement (пакеты: $PACKAGES), лог: $RESULTS_LOG"

# Быстрая проверка компиляции тестов: ошибки сборки видны до запуска (и без OOM на линковке).
if [ "${COMPLEMENT_PREBUILD:-0}" = "1" ]; then
    echo "== go vet тестовых пакетов (проверка сборки)"
    if ! ( cd "$WORKDIR" && GOFLAGS="$GOFLAGS_PARALLEL" go vet $PACKAGES ) ; then
        echo "!! тестовые пакеты Complement не собираются — см. вывод go vet выше" >&2

        exit 1
    fi
fi

run_tests() {
    local -a packages
    read -r -a packages <<< "$PACKAGES"

    if [[ -n "${COMPLEMENT_RUNNER_IMAGE:-}" ]]; then
        # В Docker executor job и dind имеют разный 127.0.0.1. Upstream публикует
        # homeserver именно туда. Исполняем suite в сети вложенного демона.
        git -C "$WORKDIR" archive HEAD | docker run --rm -i --network host \
            --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
            -e "COMPLEMENT_BASE_IMAGE=$IMAGE" -e COMPLEMENT_ALWAYS_DESTROY=true \
            -e COMPLEMENT_SPAWN_HS_TIMEOUT_SECS -e COMPLEMENT_ENABLE_DIRTY_RUNS \
            -e "GOFLAGS=$GOFLAGS_PARALLEL" -w /complement "$COMPLEMENT_RUNNER_IMAGE" \
            sh -ec 'tar -xf - && exec go test -json -count=1 "$@"' sh "-timeout=$GO_TEST_TIMEOUT" "${packages[@]}"
    else
        (
            cd "$WORKDIR" || exit 1
            GOFLAGS="$GOFLAGS_PARALLEL" COMPLEMENT_BASE_IMAGE="$IMAGE" COMPLEMENT_ALWAYS_DESTROY=true \
                go test -json -count=1 -timeout "$GO_TEST_TIMEOUT" "${packages[@]}"
        )
    fi
}

set +e
run_tests > >(tee "$RESULTS_RAW" > "$RESULTS_LOG")
test_status=$?
set -e

if [ "$test_status" -ne 0 ]; then
    echo "!! go test завершился с кодом $test_status — хвост лога:" >&2
    tail -n 60 "$RESULTS_LOG" >&2

    # Пустой/битый вывод (OOM, ошибка сборки) не должен выглядеть как «нет падений».
    if ! grep -q '"Action":"\(pass\|fail\|skip\)"' "$RESULTS_RAW" 2>/dev/null; then
        echo "!! Complement не дошёл до тестов: проверьте GOFLAGS/OOM и ошибки сборки выше" >&2

        exit "$test_status"
    fi
fi

echo "== нормализация результатов и ratchet"
update_flag=""
[ "$update_baseline" -eq 1 ] && update_flag="--update"
python3 tools/complement/complement_ratchet.py check --input "$RESULTS_RAW" \
    --output "$RESULTS_NORMALIZED" --print-pass-rate $update_flag
