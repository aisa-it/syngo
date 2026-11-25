#!/usr/bin/env bash
# Coverage-ratchet: падает, если общее покрытие или покрытие
# любого пакета из baseline ниже зафиксированного. Планка только растет.
#
#   scripts/coverage-ratchet.sh                     # прогнать тесты и проверить
#   scripts/coverage-ratchet.sh --update            # поднять baseline (понижение - отказ)
#   COVERAGE_PROFILE=cover.out COVERAGE_REUSE=1 scripts/coverage-ratchet.sh
#                                                   # проверить готовый профиль (CI)
#
# Профиль снимается так же, как в CI-джобе test: -race -shuffle=on, без БД.
# Baseline: test/coverage-baseline.txt (total и пакеты), правила -
# tools/ratchet/coverage_ratchet.py.
set -euo pipefail

cd "$(dirname "$0")/.."

PROFILE=${COVERAGE_PROFILE:-/tmp/syngo-cover.out}
command=check
[[ ${1:-} == "--update" ]] && command=update

if [[ ${COVERAGE_REUSE:-0} != 1 ]]; then
    CGO_ENABLED=1 go test -race -shuffle=on -count=1 -covermode=atomic -coverprofile="$PROFILE" ./... >/dev/null
fi

exec python3 tools/ratchet/coverage_ratchet.py "$command" --profile "$PROFILE"
