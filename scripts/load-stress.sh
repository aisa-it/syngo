#!/usr/bin/env bash
# Стрессовый прогон: открытая модель cmd/loadgen ступенями по целевой
# частоте действий до отказа. Отказ ступени - доля ошибок без 429 больше
# STRESS_MAX_ERR или полнота доставки меньше STRESS_MIN_DELIVERY.
#
#   scripts/load-stress.sh [syngo|synapse] [профиль] [ступени RPS...]
#   scripts/load-stress.sh syngo M 50 100 200 300 400 600 800
#
# Окружение: STRESS_STEP (длительность ступени, 2m), STRESS_RAMP (20s),
# STRESS_MAX_ERR (0.05), STRESS_MIN_DELIVERY (0.95), STRESS_TAG (метка отчетов).
# Итог - var/loadtest/stress-<tag>/summary.tsv: rps, фактические действия/с,
# ошибки, полнота доставки, p50/p95/p99 доставки, p95 send, /sync p95, CPU, RSS.
set -euo pipefail
cd "$(dirname "$0")/.."
SERVER=${1:-syngo}
PROFILE=${2:-M}
shift 2 || true
STEPS=("$@")
[[ ${#STEPS[@]} -gt 0 ]] || STEPS=(50 100 200 300 400 600 800)
STEP=${STRESS_STEP:-2m}
RAMP=${STRESS_RAMP:-20s}
MAX_ERR=${STRESS_MAX_ERR:-0.05}
MIN_DELIVERY=${STRESS_MIN_DELIVERY:-0.95}
TAG=${STRESS_TAG:-$SERVER-$PROFILE-$(date +%Y%m%d-%H%M%S)}
OUT=var/loadtest/stress-$TAG
mkdir -p "$OUT"
printf 'rps\tactions_per_s\terror_rate\tdelivery\tdeliv_p50_ms\tdeliv_p95_ms\tdeliv_p99_ms\tsend_p95_ms\tsync_p95_ms\tcpu_avg_pct\trss_max_kb\tload1\n' >"$OUT/summary.tsv"

for rps in "${STEPS[@]}"; do
	name=stress-$TAG/rps-$rps
	echo "== ступень $rps действий/с ($STEP)"
	load1=$(cut -d' ' -f1 /proc/loadavg)
	set +e
	bash scripts/load-run.sh "$SERVER" "$PROFILE" -mode rps -rps "$rps" -duration "$STEP" -ramp "$RAMP" \
		-max-error-rate 1 -min-delivery 0 -report-name "$name" >"$OUT/rps-$rps.log" 2>&1
	set -e
	report=var/loadtest/$name/report.json
	if [[ ! -f $report ]]; then
		echo "нет отчета ступени $rps - останов"; break
	fi
	row=$(python3 - "$report" "$rps" "$load1" <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
s, d, e, srv, pr = r.get("summary", {}), r.get("delivery", {}), r.get("endpoints", {}), r.get("server", {}), r.get("process", {})
ack = d.get("from_ack", {})
print("\t".join(str(x) for x in [
    sys.argv[2], s.get("actions_rps", 0), round(s.get("error_rate", 0), 4), round(d.get("completeness", 0), 3),
    round(ack.get("p50_ms", 0), 1), round(ack.get("p95_ms", 0), 1), round(ack.get("p99_ms", 0), 1),
    round(e.get("send_text", {}).get("p95_ms", 0), 1), round(srv.get("sync_p95_ms", 0), 1),
    round(pr.get("cpu_avg_pct", 0), 1), pr.get("rss_max_kb", 0), sys.argv[3],
]))
PY
)
	echo "$row" >>"$OUT/summary.tsv"
	echo "$row"
	err=$(cut -f3 <<<"$row")
	del=$(cut -f4 <<<"$row")
	if python3 -c "import sys; sys.exit(0 if float('$err') > float('$MAX_ERR') or float('$del') < float('$MIN_DELIVERY') else 1)"; then
		echo "== отказ на ступени $rps (ошибки $err, доставка $del) - останов"
		break
	fi
done
echo "итог: $OUT/summary.tsv"
