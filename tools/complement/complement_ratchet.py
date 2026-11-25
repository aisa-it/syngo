#!/usr/bin/env python3
"""Нормализация результатов Complement и ratchet-проверка.

Две команды:

    normalize — читает `go test -json` из stdin (или файла) и пишет стабильный JSONL
                с парами {Action, Test}: pass/fail/skip, отсортированными по Test,
                в --output. Baseline normalize не трогает (раньше перезаписывал его
                без проверки: 349 записей -> 1).
    check     — сравнивает текущие результаты с baseline: ранее проходивший тест стал
                fail/skip или пропал → exit 1; pass стало меньше, чем в baseline, → exit 1.
                Исключения берутся из allowlist (тест - причина - дата).

Дополнительно:
    --update  после успешной проверки обновляет baseline (ratchet только растет:
              при регрессии или сокращении набора тестов baseline не меняется)
    --output  куда записать нормализованный текущий прогон (артефакт CI)
    --allowlist путь к allowlist (по умолчанию test/complement/allowlist.txt)
    --print-pass-rate печатает pass rate по группам (префикс пакета/первый компонент имени)

Формат результатов (по строке на тест, отсортировано по Test):

    {"Action":"pass","Test":"TestBasicRoomCreation"}
"""

from __future__ import annotations

import argparse
import datetime
import json
import pathlib
import sys

REPO = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_BASELINE = REPO / "test" / "complement" / "results.jsonl"
DEFAULT_ALLOWLIST = REPO / "test" / "complement" / "allowlist.txt"

ACTIONS = ("pass", "fail", "skip")


def normalize_stream(lines) -> list[dict]:
    """Превращает поток событий `go test -json` в стабильные записи {Action, Test}."""
    by_test: dict[str, str] = {}

    for line in lines:
        line = line.strip()
        if not line:
            continue

        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            # Complement может писать не-JSON строки (логи сборки) — пропускаем.
            continue

        test = event.get("Test")
        action = event.get("Action")

        if not test or action not in ACTIONS:
            continue

        # Один тест может дать несколько событий: финальный статус — последний.
        by_test[test] = action

    return [{"Action": by_test[name], "Test": name} for name in sorted(by_test)]


def load_jsonl(path: pathlib.Path) -> list[dict]:
    if not path.exists():
        return []

    records = []

    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue

        records.append(json.loads(line))

    return records


def write_jsonl(path: pathlib.Path, records: list[dict], header: str = "") -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    body = "".join(json.dumps(record, ensure_ascii=False, sort_keys=True) + "\n" for record in records)
    path.write_text(header + body, encoding="utf-8")


def baseline_header(today: datetime.date, records: list[dict]) -> str:
    """Комментарий с датой снятия: mtime файла после git checkout ничего не значит."""
    passed = sum(1 for record in records if record["Action"] == "pass")

    return f"# measured: {today.isoformat()} pass {passed}/{len(records)}\n"


def baseline_measured(path: pathlib.Path) -> datetime.date | None:
    """Дата снятия baseline из заголовка `# measured: YYYY-MM-DD`."""
    if not path.exists():
        return None

    with path.open(encoding="utf-8") as handle:
        first = handle.readline().strip()

    if first.startswith("# measured:"):
        try:
            return datetime.date.fromisoformat(first.split()[2])
        except (IndexError, ValueError):
            return None

    return None


def load_allowlist(path: pathlib.Path, today: str) -> dict[str, str]:
    """Читает allowlist: формат «Тест | причина | YYYY-MM-DD».

    Строки без причины или с просроченной датой пересмотра считаются ошибкой конфигурации:
    исключение живет только с обоснованием и датой пересмотра.
    """
    allowed: dict[str, str] = {}

    if not path.exists():
        return allowed

    problems = []

    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue

        parts = [part.strip() for part in line.split("|")]
        if len(parts) != 3:
            problems.append(f"строка без причины/даты: {raw!r}")
            continue

        test, reason, review_at = parts
        if not reason:
            problems.append(f"пустая причина: {test}")
            continue

        if review_at < today:
            problems.append(f"просрочена дата пересмотра {review_at}: {test}")

        allowed[test] = reason

    if problems:
        raise SystemExit("ratchet: невалидный allowlist:\n  " + "\n  ".join(problems))

    return allowed


def check(baseline: list[dict], current: list[dict], allowlist: dict[str, str]) -> list[str]:
    """Возвращает список регрессий: тест проходил в baseline, но теперь fail/skip."""
    current_by_test = {record["Test"]: record["Action"] for record in current}
    regressions = []

    for record in baseline:
        if record["Action"] != "pass":
            continue

        test = record["Test"]
        if test in allowlist:
            continue

        action = current_by_test.get(test)
        if action != "pass":
            regressions.append(f"{test}: было pass, стало {action or 'отсутствует'}")

    return regressions


def pass_rate(records: list[dict]) -> dict[str, tuple[int, int]]:
    """Pass rate по группам: первый компонент имени теста (пакет/группа)."""
    groups: dict[str, list[int]] = {}

    for record in records:
        group = record["Test"].split("/")[0]
        counters = groups.setdefault(group, [0, 0])
        counters[1] += 1

        if record["Action"] == "pass":
            counters[0] += 1

    return {group: (passed, total) for group, (passed, total) in sorted(groups.items())}


def print_pass_rate(records: list[dict], out=sys.stdout) -> None:
    for group, (passed, total) in pass_rate(records).items():
        share = 100.0 * passed / total if total else 0.0
        print(f"  {group}: {passed}/{total} ({share:.1f}%)", file=out)


def baseline_age_days(baseline: pathlib.Path, today: datetime.date) -> int | None:
    """Возраст baseline в днях: по заголовку `# measured:`, иначе по mtime файла
    (None, если файла нет).

    Ratchet сравнивает прогон с baseline, но не замечает, что baseline устарел:
    тесты, закрытые после его снятия, продолжают числиться падениями, а новые
    регрессии на их фоне не видны.
    """
    measured = baseline_measured(baseline)
    if measured is not None:
        return (today - measured).days

    try:
        modified = datetime.date.fromtimestamp(baseline.stat().st_mtime)
    except OSError:
        return None

    return (today - modified).days


def shrink_problems(baseline: list[dict], current: list[dict], allowlist: dict[str, str]) -> list[str]:
    """Набор тестов и число pass не сокращаются относительно baseline.

    Тест из baseline, которого нет в прогоне (не собрался пакет, не поднялся
    контейнер), - сокращение набора; pass меньше, чем в baseline (без allowlist), -
    регрессия, даже если отдельные тесты переименованы.
    """
    problems = []
    current_tests = {record["Test"] for record in current}
    missing = sorted(record["Test"] for record in baseline if record["Test"] not in current_tests)

    if missing:
        problems.append(f"в прогоне нет {len(missing)} тестов из baseline (первые: {', '.join(missing[:5])})")

    base_pass = sum(1 for r in baseline if r["Action"] == "pass" and r["Test"] not in allowlist)
    curr_pass = sum(1 for r in current if r["Action"] == "pass" and r["Test"] not in allowlist)

    if curr_pass < base_pass:
        problems.append(f"pass {curr_pass} < baseline {base_pass}")

    return problems


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Complement: нормализация результатов и ratchet")
    parser.add_argument("command", choices=("normalize", "check"))
    parser.add_argument("--input", type=pathlib.Path, help="файл с go test -json (по умолчанию stdin)")
    parser.add_argument("--output", type=pathlib.Path, help="куда записать нормализованный прогон")
    parser.add_argument("--baseline", type=pathlib.Path, default=DEFAULT_BASELINE)
    parser.add_argument("--allowlist", type=pathlib.Path, default=DEFAULT_ALLOWLIST)
    parser.add_argument("--update", action="store_true",
                        help="после успешной проверки обновить baseline текущими результатами")
    parser.add_argument("--print-pass-rate", action="store_true")
    # Дата по умолчанию - сегодня: зашитая дата принимала просроченные записи allowlist.
    parser.add_argument("--today", default=datetime.date.today().isoformat(),
                        help="дата для проверки allowlist (YYYY-MM-DD, по умолчанию сегодня)")
    parser.add_argument("--max-age-days", type=int, default=7,
                        help="после скольких дней baseline считается устаревшим (0 — не проверять)")
    args = parser.parse_args(argv)

    today = datetime.date.fromisoformat(args.today)

    if args.command == "normalize":
        if args.output is None:
            print("normalize: нужен --output (baseline обновляет только `check --update`)", file=sys.stderr)

            return 2

        if args.output.resolve() == args.baseline.resolve():
            print("normalize: запись в baseline запрещена, используйте `check --update`", file=sys.stderr)

            return 2

        if args.input:
            lines = args.input.read_text(encoding="utf-8").splitlines()
        else:
            lines = sys.stdin

        records = normalize_stream(lines)
        write_jsonl(args.output, records)
        print(f"normalize: {len(records)} тестов → {args.output}")

        if args.print_pass_rate:
            print_pass_rate(records)

        return 0

    if args.input is None:
        print("check: нужен --input с результатами прогона", file=sys.stderr)

        return 2

    if args.max_age_days > 0:
        if age := baseline_age_days(args.baseline, today):
            if age > args.max_age_days:
                print(f"предупреждение: baseline старше {args.max_age_days} дней ({age}); "
                      f"падения могли быть закрыты — переснимите прогон", file=sys.stderr)

    current = normalize_stream(args.input.read_text(encoding="utf-8").splitlines())
    baseline = load_jsonl(args.baseline)
    allowlist = load_allowlist(args.allowlist, args.today)

    if args.output:
        write_jsonl(args.output, current)

    if not current:
        print("ratchet: в прогоне нет ни одного теста - Complement не дошел до тестов", file=sys.stderr)

        return 1

    if args.print_pass_rate:
        print("pass rate по группам:")
        print_pass_rate(current)

    if not baseline and not args.update:
        # Fail-closed: пустой baseline не должен давать зелёный job — иначе ratchet
        # «работает» только на бумаге. Планку фиксирует только явный --update.
        print("ratchet: baseline пуст — заполните его прогоном с `--update`", file=sys.stderr)

        return 1

    regressions = check(baseline, current, allowlist)
    shrink = shrink_problems(baseline, current, allowlist)

    base_pass = sum(1 for record in baseline if record["Action"] == "pass")
    curr_pass = sum(1 for record in current if record["Action"] == "pass")
    print(f"pass: {curr_pass}/{len(current)} (baseline {base_pass}/{len(baseline)})")

    if regressions or shrink:
        if regressions:
            print("ratchet: регрессии (тест проходил, теперь нет):", file=sys.stderr)
            for line in regressions:
                print(f"  {line}", file=sys.stderr)

        for line in shrink:
            print(f"ratchet: {line}", file=sys.stderr)

        if args.update:
            print("ratchet: baseline не обновлен", file=sys.stderr)

        return 1

    fixed = sorted(test for test in allowlist if any(
        record["Test"] == test and record["Action"] == "pass" for record in current))
    for test in fixed:
        print(f"allowlist: {test} снова проходит - уберите исключение")

    print(f"ratchet: регрессий нет ({len(baseline)} записей в baseline, {len(allowlist)} исключений)")

    if args.update:
        write_jsonl(args.baseline, current, baseline_header(today, current))
        print(f"baseline обновлён: {len(current)} тестов → {args.baseline}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
