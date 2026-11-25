#!/usr/bin/env python3
"""Perf-ratchet /sync: сравнение метрик cmd/syncload с baseline.

    check  --baseline bench/sync-baseline.json --current metrics.json
    update --baseline bench/sync-baseline.json --current metrics.json
    profile --baseline bench/sync-baseline.json     печать профиля для запуска syncload

Что ловит (каждое - exit 1):
  - профиль прогона (комнаты, события, участники, клиенты, итерации) не совпадает
    с baseline: раньше baseline был снят на 500x10x10, а CI гонял 1000x20x50;
  - сценарий из baseline пропал, в нем 0 замеров или есть ошибки сборки /sync
    (при полном отказе загрузчика раньше выходил p99=0 и зеленый гейт);
  - SQL-запросов на sync больше, чем в baseline, - в любом сценарии, без исключений
    для быстрых (ADR-0005: число запросов не растет);
  - p99 выше max(baseline * (1 + P99_TOLERANCE), baseline + P99_SLACK_MS). Абсолютный
    запас заменяет прежнее исключение сценариев короче 20 мс из проверки: рост
    4.6 -> 45 мс (+873%) теперь регрессия;
  - время сравнивается только на том же железе (модель CPU и число ядер): цифры с
    другого хоста ничего не доказывают. На другом железе - отказ с подсказкой снять
    baseline на этом раннере (`update`), а не молчаливый зеленый.

Пороги - константы здесь, а не переменные окружения: гейт нельзя ослабить из
настроек джобы (раньше TOLERANCE=100 давал ok).
"""

from __future__ import annotations

import argparse
import json
import pathlib
import shutil
import sys

P99_TOLERANCE = 0.25
P99_SLACK_MS = 10.0
SQL_EPSILON = 0.01

PROFILE_KEYS = ("rooms", "events_per_room", "members_per_room", "users", "clients", "iterations", "per_room_limit")
HARDWARE_KEYS = ("cpu_model", "cpus")


def load(path: pathlib.Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise SystemExit(f"perf-ratchet: нет файла {path}") from exc


def run_problems(metrics: dict) -> list[str]:
    """Прогон без ошибок и с замерами в каждом сценарии."""
    problems = []
    scenarios = metrics.get("scenarios") or {}

    if not scenarios:
        problems.append("в метриках нет ни одного сценария")

    for name, values in sorted(scenarios.items()):
        if values.get("errors", 0) > 0:
            problems.append(f"{name}: {values['errors']} ошибок сборки /sync")

        if values.get("samples", 0) <= 0 or values.get("p99_ms", 0) <= 0:
            problems.append(f"{name}: нет замеров (samples={values.get('samples', 0)}, p99={values.get('p99_ms', 0)})")

    return problems


def compare(baseline: dict, current: dict) -> list[str]:
    problems = run_problems(current)

    base_profile = {key: baseline.get("profile", {}).get(key) for key in PROFILE_KEYS}
    curr_profile = {key: current.get("profile", {}).get(key) for key in PROFILE_KEYS}
    if base_profile != curr_profile:
        problems.append(f"профиль прогона {curr_profile} не совпадает с baseline {base_profile}")

        return problems

    base_hw = {key: baseline.get("hardware", {}).get(key) for key in HARDWARE_KEYS}
    curr_hw = {key: current.get("hardware", {}).get(key) for key in HARDWARE_KEYS}
    same_hardware = base_hw == curr_hw and None not in base_hw.values()

    print(f"{'сценарий':<13} {'p99 base':>10} {'p99 now':>10} {'лимит':>10} {'SQL base':>9} {'SQL now':>8}")

    for name, before in sorted(baseline.get("scenarios", {}).items()):
        after = current.get("scenarios", {}).get(name)
        if not after:
            problems.append(f"{name}: сценарий пропал из прогона")
            continue

        base_p99 = float(before.get("p99_ms", 0))
        curr_p99 = float(after.get("p99_ms", 0))
        limit = max(base_p99 * (1 + P99_TOLERANCE), base_p99 + P99_SLACK_MS)
        base_sql = float(before.get("sql_per_sync", 0))
        curr_sql = float(after.get("sql_per_sync", 0))

        print(f"{name:<13} {base_p99:>8.1f}мс {curr_p99:>8.1f}мс {limit:>8.1f}мс {base_sql:>9.1f} {curr_sql:>8.1f}")

        if curr_sql > base_sql + SQL_EPSILON:
            problems.append(f"{name}: SQL на sync вырос {base_sql:g} → {curr_sql:g}")

        if same_hardware and curr_p99 > limit:
            problems.append(f"{name}: p99 {curr_p99:.1f} мс > лимита {limit:.1f} мс (baseline {base_p99:.1f} мс)")

    if not same_hardware:
        problems.append(
            f"baseline снят на другом железе ({base_hw}, сейчас {curr_hw}): время не сравнимо. "
            "Снимите baseline на этом железе (make perf-baseline) и закоммитьте"
        )

    return problems


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="perf-ratchet /sync")
    parser.add_argument("command", choices=("check", "update", "profile"))
    parser.add_argument("--baseline", type=pathlib.Path, required=True)
    parser.add_argument("--current", type=pathlib.Path)
    args = parser.parse_args(argv)

    if args.command == "profile":
        profile = load(args.baseline).get("profile", {})
        missing = [key for key in PROFILE_KEYS if key not in profile]
        if missing:
            print(f"perf-ratchet: в профиле baseline нет {missing}", file=sys.stderr)

            return 1

        # Формат для `eval` в bash: ROOMS=... EVENTS=... (значения - целые числа).
        print(" ".join(f"PERF_{key.upper()}={int(profile[key])}" for key in PROFILE_KEYS))

        return 0

    if args.current is None:
        print("perf-ratchet: нужен --current", file=sys.stderr)

        return 2

    current = load(args.current)

    if args.command == "update":
        problems = run_problems(current)
        if problems:
            print("perf-ratchet: прогон с ошибками, baseline не обновлен:", file=sys.stderr)
            for line in problems:
                print(f"  {line}", file=sys.stderr)

            return 1

        shutil.copyfile(args.current, args.baseline)
        print(f"perf-ratchet: baseline обновлен → {args.baseline}")

        return 0

    problems = compare(load(args.baseline), current)
    if problems:
        print("\nperf-ratchet: регрессии:", file=sys.stderr)
        for line in problems:
            print(f"  {line}", file=sys.stderr)

        return 1

    print("\nperf-ratchet: ok")

    return 0


if __name__ == "__main__":
    sys.exit(main())
