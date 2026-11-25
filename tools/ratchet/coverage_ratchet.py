#!/usr/bin/env python3
"""Coverage-ratchet по пакетам и общий.

Читает профиль `go test -coverprofile` и сравнивает покрытие statements с baseline
`test/coverage-baseline.txt`: общий процент и каждый пакет из baseline не ниже
зафиксированного. Планка только растет.

    check  --profile cover.out             проверить (exit 1 при регрессе)
    update --profile cover.out             поднять baseline до текущих значений;
                                           отказ, если что-то ниже baseline

Понизить baseline можно только правкой файла руками в pull request - так понижение
видно в diff и проходит ревью (раньше `--update` молча понижал планку).

Формат baseline (по строке, отсортировано):

    total 35.7
    internal/api 35.7
    internal/auth 88.7

Допуск TOLERANCE_PP - шум от `-race -shuffle=on` (порядок тестов и тайминги
горутин меняют покрытие веток на десятые доли). Порог задан константой, а не
переменной окружения: гейт нельзя ослабить из настроек джобы.
"""

from __future__ import annotations

import argparse
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_BASELINE = REPO / "test" / "coverage-baseline.txt"

# Допустимое падение в процентных пунктах на пакет и на общий процент.
TOLERANCE_PP = 0.2

TOTAL = "total"


def module_path(repo: pathlib.Path = REPO) -> str:
    for line in (repo / "go.mod").read_text(encoding="utf-8").splitlines():
        if line.startswith("module "):
            return line.split()[1]

    raise SystemExit("coverage-ratchet: в go.mod нет module")


def parse_profile(lines, module: str) -> dict[str, tuple[int, int]]:
    """Профиль -> {пакет: (покрыто statements, всего statements)} и TOTAL.

    Один и тот же блок может встретиться несколько раз (несколько тестовых
    бинарников): блок покрыт, если покрыт хотя бы в одном.
    """
    blocks: dict[str, tuple[int, bool]] = {}
    seen_mode = False

    for raw in lines:
        line = raw.strip()
        if not line:
            continue

        if line.startswith("mode:"):
            seen_mode = True
            continue

        try:
            location, statements, count = line.rsplit(" ", 2)
            statements_n, count_n = int(statements), int(count)
        except ValueError as exc:
            raise SystemExit(f"coverage-ratchet: битая строка профиля: {line!r}") from exc

        prev = blocks.get(location)
        covered = count_n > 0 or (prev is not None and prev[1])
        blocks[location] = (statements_n, covered)

    if not seen_mode:
        raise SystemExit("coverage-ratchet: это не профиль покрытия (нет строки mode:)")

    result: dict[str, list[int]] = {}
    prefix = module + "/"

    for location, (statements_n, covered) in blocks.items():
        file_path = location.split(":", 1)[0]
        package = file_path.rsplit("/", 1)[0]
        if package.startswith(prefix):
            package = package[len(prefix):]
        elif package == module:
            package = "."

        for key in (package, TOTAL):
            counters = result.setdefault(key, [0, 0])
            counters[1] += statements_n
            if covered:
                counters[0] += statements_n

    return {key: (covered, total) for key, (covered, total) in result.items()}


def percentages(counters: dict[str, tuple[int, int]]) -> dict[str, float]:
    # Округление как у `go tool cover -func` (одна цифра после точки).
    return {
        key: round(100.0 * covered / total, 1) if total else 0.0
        for key, (covered, total) in counters.items()
    }


def load_baseline(path: pathlib.Path) -> dict[str, float]:
    if not path.exists():
        raise SystemExit(f"coverage-ratchet: нет baseline {path}")

    baseline: dict[str, float] = {}

    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue

        parts = line.split()
        if len(parts) != 2:
            raise SystemExit(f"coverage-ratchet: строка baseline не «пакет процент»: {raw!r}")

        baseline[parts[0]] = float(parts[1])

    if TOTAL not in baseline:
        raise SystemExit(f"coverage-ratchet: в baseline нет строки «{TOTAL} N»")

    return baseline


def write_baseline(path: pathlib.Path, values: dict[str, float]) -> None:
    header = (
        "# Coverage-ratchet: покрытие statements без БД, `go test -race -shuffle=on`.\n"
        "# Обновлять только `make coverage-baseline` (планка растет); понижение - правкой руками в PR.\n"
    )
    keys = [TOTAL] + sorted(key for key in values if key != TOTAL)
    body = "".join(f"{key} {values[key]:.1f}\n" for key in keys)
    path.write_text(header + body, encoding="utf-8")


def compare(baseline: dict[str, float], current: dict[str, float]) -> list[str]:
    """Регрессии: пакет из baseline пропал или его покрытие ниже baseline - допуск."""
    problems = []

    for key, floor in sorted(baseline.items()):
        value = current.get(key)
        if value is None:
            problems.append(f"{key}: нет в профиле (пакет удален или тесты не запускались), baseline {floor:.1f}%")
            continue

        if value + TOLERANCE_PP < floor:
            problems.append(f"{key}: {value:.1f}% < baseline {floor:.1f}% (допуск {TOLERANCE_PP} п.п.)")

    return problems


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="coverage-ratchet по пакетам")
    parser.add_argument("command", choices=("check", "update"))
    parser.add_argument("--profile", type=pathlib.Path, required=True)
    parser.add_argument("--baseline", type=pathlib.Path, default=DEFAULT_BASELINE)
    parser.add_argument("--module", default=None, help="путь модуля (по умолчанию из go.mod)")
    args = parser.parse_args(argv)

    module = args.module or module_path()
    current = percentages(parse_profile(args.profile.read_text(encoding="utf-8").splitlines(), module))

    if TOTAL not in current:
        print("coverage-ratchet: профиль пуст - тесты не запускались?", file=sys.stderr)

        return 1

    if args.command == "update" and not args.baseline.exists():
        write_baseline(args.baseline, current)
        print(f"coverage-ratchet: baseline создан, total {current[TOTAL]:.1f}%")

        return 0

    baseline = load_baseline(args.baseline)
    problems = compare(baseline, current)

    print(f"покрытие: total {current[TOTAL]:.1f}% (baseline {baseline[TOTAL]:.1f}%)")

    for key in sorted(set(current) - set(baseline)):
        print(f"  новый пакет вне baseline: {key} {current[key]:.1f}%")

    if problems:
        print("coverage-ratchet: покрытие упало:", file=sys.stderr)
        for line in problems:
            print(f"  {line}", file=sys.stderr)

        if args.command == "update":
            print("coverage-ratchet: baseline не обновлен - понижение только правкой файла в PR", file=sys.stderr)

        return 1

    if args.command == "update":
        # Планка не опускается: в пределах допуска остается прежнее значение.
        merged = {key: max(value, baseline.get(key, 0.0)) for key, value in current.items()}
        write_baseline(args.baseline, merged)
        print(f"coverage-ratchet: baseline обновлен ({len(merged)} записей)")

        return 0

    print("coverage-ratchet: ok")

    return 0


if __name__ == "__main__":
    sys.exit(main())
