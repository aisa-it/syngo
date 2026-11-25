"""Тесты perf-ratchet: гейт падает на искусственно ухудшенных метриках."""

from __future__ import annotations

import copy
import io
import json
import pathlib
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import perf_ratchet as pr  # noqa: E402

BASE = {
    "profile": {"rooms": 500, "events_per_room": 10, "members_per_room": 5, "users": 10,
                "clients": 10, "iterations": 30, "per_room_limit": 20},
    "hardware": {"cpu_model": "Xeon", "cpus": 8},
    "scenarios": {
        "initial": {"p99_ms": 300.0, "sql_per_sync": 3, "samples": 300, "errors": 0},
        "empty": {"p99_ms": 4.6, "sql_per_sync": 2, "samples": 300, "errors": 0},
        "http-delta": {"p99_ms": 40.0, "sql_per_sync": 14, "samples": 300, "errors": 0},
    },
}


def quiet(func, *args):
    with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
        return func(*args)


class CompareTest(unittest.TestCase):
    def worse(self, mutate) -> list[str]:
        current = copy.deepcopy(BASE)
        mutate(current)

        return quiet(pr.compare, BASE, current)

    def test_same_metrics_pass(self):
        self.assertEqual(self.worse(lambda c: None), [])

    def test_noise_within_limit_passes(self):
        def mutate(c):
            c["scenarios"]["initial"]["p99_ms"] = 360.0  # +20%
            c["scenarios"]["empty"]["p99_ms"] = 12.0     # +7.4 мс, в абсолютном запасе

        self.assertEqual(self.worse(mutate), [])

    def test_p99_regression_fails(self):
        self.assertTrue(self.worse(lambda c: c["scenarios"]["initial"].update(p99_ms=400.0)))

    def test_small_scenario_regression_fails(self):
        # 4.6 -> 45 мс (+873%): раньше сценарии < 20 мс не проверялись.
        self.assertTrue(self.worse(lambda c: c["scenarios"]["empty"].update(p99_ms=45.0)))

    def test_sql_growth_fails_in_fast_scenario(self):
        self.assertTrue(self.worse(lambda c: c["scenarios"]["empty"].update(sql_per_sync=3)))

    def test_loader_failure_fails(self):
        # Полный отказ: 0 замеров, p99=0 - раньше гейт был зеленым.
        self.assertTrue(self.worse(lambda c: c["scenarios"]["http-delta"].update(
            p99_ms=0, samples=0, errors=300)))

    def test_missing_scenario_fails(self):
        self.assertTrue(self.worse(lambda c: c["scenarios"].pop("empty")))

    def test_profile_mismatch_fails(self):
        self.assertTrue(self.worse(lambda c: c["profile"].update(rooms=1000, clients=50)))

    def test_other_hardware_fails_but_checks_sql(self):
        problems = self.worse(lambda c: c["hardware"].update(cpu_model="EPYC"))
        self.assertEqual(len(problems), 1)
        self.assertIn("другом железе", problems[0])

        problems = self.worse(lambda c: (c["hardware"].update(cpu_model="EPYC"),
                                         c["scenarios"]["initial"].update(sql_per_sync=4)))
        self.assertTrue(any("SQL" in line for line in problems))


class CliTest(unittest.TestCase):
    def setUp(self):
        self.dir = pathlib.Path(tempfile.mkdtemp())
        self.baseline = self.dir / "baseline.json"
        self.current = self.dir / "current.json"
        self.baseline.write_text(json.dumps(BASE), encoding="utf-8")

    def write_current(self, metrics: dict) -> None:
        self.current.write_text(json.dumps(metrics), encoding="utf-8")

    def test_check_exit_codes(self):
        self.write_current(BASE)
        self.assertEqual(quiet(pr.main, ["check", "--baseline", str(self.baseline), "--current", str(self.current)]), 0)

        worse = copy.deepcopy(BASE)
        worse["scenarios"]["initial"]["sql_per_sync"] = 103
        self.write_current(worse)
        self.assertEqual(quiet(pr.main, ["check", "--baseline", str(self.baseline), "--current", str(self.current)]), 1)

    def test_update_refuses_failed_run(self):
        broken = copy.deepcopy(BASE)
        broken["scenarios"]["initial"].update(errors=5)
        self.write_current(broken)
        before = self.baseline.read_text(encoding="utf-8")
        self.assertEqual(quiet(pr.main, ["update", "--baseline", str(self.baseline), "--current", str(self.current)]), 1)
        self.assertEqual(self.baseline.read_text(encoding="utf-8"), before)

    def test_profile_prints_env(self):
        out = io.StringIO()
        with redirect_stdout(out):
            self.assertEqual(pr.main(["profile", "--baseline", str(self.baseline)]), 0)
        self.assertIn("PERF_ROOMS=500", out.getvalue())
        self.assertIn("PERF_CLIENTS=10", out.getvalue())


if __name__ == "__main__":
    unittest.main()
