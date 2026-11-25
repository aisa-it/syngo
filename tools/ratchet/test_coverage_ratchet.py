"""Тесты coverage-ratchet: гейт падает на искусственно ухудшенном профиле."""

from __future__ import annotations

import io
import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import coverage_ratchet as cr  # noqa: E402

MODULE = "example.org/m"


def profile(*blocks: tuple[str, int, int]) -> str:
    lines = ["mode: atomic"]
    for location, statements, count in blocks:
        lines.append(f"{MODULE}/{location} {statements} {count}")

    return "\n".join(lines) + "\n"


GOOD = profile(
    ("internal/a/a.go:1.1,2.2", 6, 1),
    ("internal/a/a.go:3.1,4.2", 4, 0),
    ("internal/b/b.go:1.1,2.2", 10, 3),
)


class ParseTest(unittest.TestCase):
    def test_per_package_and_total(self):
        got = cr.percentages(cr.parse_profile(GOOD.splitlines(), MODULE))
        self.assertEqual(got, {"internal/a": 60.0, "internal/b": 100.0, cr.TOTAL: 80.0})

    def test_duplicate_block_counts_once(self):
        text = profile(("internal/a/a.go:1.1,2.2", 5, 0), ("internal/a/a.go:1.1,2.2", 5, 2))
        self.assertEqual(cr.parse_profile(text.splitlines(), MODULE)["internal/a"], (5, 5))

    def test_not_a_profile(self):
        with self.assertRaises(SystemExit):
            cr.parse_profile(["garbage 1 2"], MODULE)


class RatchetTest(unittest.TestCase):
    def setUp(self):
        self.dir = pathlib.Path(tempfile.mkdtemp())
        self.baseline = self.dir / "baseline.txt"
        self.baseline.write_text("total 80.0\ninternal/a 60.0\ninternal/b 100.0\n", encoding="utf-8")

    def run_cli(self, command: str, text: str) -> int:
        path = self.dir / "cover.out"
        path.write_text(text, encoding="utf-8")
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            return cr.main([command, "--profile", str(path), "--baseline", str(self.baseline), "--module", MODULE])

    def test_same_profile_passes(self):
        self.assertEqual(self.run_cli("check", GOOD), 0)

    def test_package_regression_fails(self):
        # internal/a: 60% -> 40%, общий процент тоже ниже.
        worse = profile(
            ("internal/a/a.go:1.1,2.2", 4, 1),
            ("internal/a/a.go:3.1,4.2", 6, 0),
            ("internal/b/b.go:1.1,2.2", 10, 3),
        )
        self.assertEqual(self.run_cli("check", worse), 1)

    def test_package_regression_hidden_by_total_fails(self):
        # Общий процент вырос за счет нового пакета, но internal/a упал - гейт ловит.
        worse = GOOD.replace("internal/a/a.go:1.1,2.2 6 1", "internal/a/a.go:1.1,2.2 6 0") + (
            f"{MODULE}/internal/c/c.go:1.1,2.2 100 1\n"
        )
        self.assertEqual(self.run_cli("check", worse), 1)

    def test_missing_package_fails(self):
        only_a = profile(("internal/a/a.go:1.1,2.2", 6, 1), ("internal/a/a.go:3.1,4.2", 4, 0))
        self.assertEqual(self.run_cli("check", only_a), 1)

    def test_update_refuses_to_lower(self):
        worse = profile(
            ("internal/a/a.go:1.1,2.2", 1, 1),
            ("internal/a/a.go:3.1,4.2", 9, 0),
            ("internal/b/b.go:1.1,2.2", 10, 3),
        )
        before = self.baseline.read_text(encoding="utf-8")
        self.assertEqual(self.run_cli("update", worse), 1)
        self.assertEqual(self.baseline.read_text(encoding="utf-8"), before)

    def test_update_raises_and_adds_packages(self):
        better = GOOD.replace("internal/a/a.go:3.1,4.2 4 0", "internal/a/a.go:3.1,4.2 4 1") + (
            f"{MODULE}/internal/c/c.go:1.1,2.2 2 1\n"
        )
        self.assertEqual(self.run_cli("update", better), 0)
        values = cr.load_baseline(self.baseline)
        self.assertEqual(values["internal/a"], 100.0)
        self.assertEqual(values["internal/c"], 100.0)

    def test_empty_profile_fails(self):
        self.assertEqual(self.run_cli("check", "mode: atomic\n"), 1)


if __name__ == "__main__":
    unittest.main()
