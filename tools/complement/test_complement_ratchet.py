"""Тесты нормализации и ratchet (stdlib, без Docker и сети).

Запуск: python3 -m unittest discover -s tools/complement -p 'test_*.py'
"""

import datetime
import io
import json
import os
import pathlib
import tempfile
import unittest

import complement_ratchet as ratchet


def events(*records) -> list[str]:
    return [json.dumps(record) for record in records]


class NormalizeTest(unittest.TestCase):
    def test_keeps_last_status_per_test(self):
        lines = events(
            {"Action": "run", "Test": "TestA"},
            {"Action": "pass", "Test": "TestA"},
            {"Action": "output", "Test": "TestA", "Output": "PASS\n"},
            {"Action": "fail", "Test": "TestB"},
            {"Action": "skip", "Test": "TestC"},
            {"Action": "pass"},  # пакетное событие без теста — пропускаем
        )
        records = ratchet.normalize_stream(lines)
        self.assertEqual(
            records,
            [
                {"Action": "pass", "Test": "TestA"},
                {"Action": "fail", "Test": "TestB"},
                {"Action": "skip", "Test": "TestC"},
            ],
        )

    def test_ignores_non_json_lines(self):
        records = ratchet.normalize_stream(["docker build output", '{"Action":"pass","Test":"TestA"}'])
        self.assertEqual(records, [{"Action": "pass", "Test": "TestA"}])

    def test_output_is_stable(self):
        lines = events({"Action": "pass", "Test": "TestZ"}, {"Action": "pass", "Test": "TestA"})
        first = ratchet.normalize_stream(lines)

        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "results.jsonl"
            ratchet.write_jsonl(path, first)
            second = ratchet.load_jsonl(path)

        self.assertEqual(first, second)
        self.assertEqual([record["Test"] for record in second], ["TestA", "TestZ"])


class RatchetTest(unittest.TestCase):
    BASELINE = [
        {"Action": "pass", "Test": "TestA"},
        {"Action": "pass", "Test": "TestB"},
        {"Action": "skip", "Test": "TestC"},
    ]

    def test_no_regressions(self):
        current = [
            {"Action": "pass", "Test": "TestA"},
            {"Action": "pass", "Test": "TestB"},
            {"Action": "fail", "Test": "TestC"},  # в baseline был skip — не регрессия
        ]
        self.assertEqual(ratchet.check(self.BASELINE, current, {}), [])

    def test_regression_is_reported(self):
        current = [{"Action": "fail", "Test": "TestA"}, {"Action": "pass", "Test": "TestB"}]
        regressions = ratchet.check(self.BASELINE, current, {})
        self.assertEqual(len(regressions), 1)
        self.assertIn("TestA", regressions[0])

    def test_missing_test_is_regression(self):
        current = [{"Action": "pass", "Test": "TestB"}]
        regressions = ratchet.check(self.BASELINE, current, {})
        self.assertEqual(len(regressions), 1)
        self.assertIn("отсутствует", regressions[0])

    def test_allowlist_excludes_test(self):
        current = [{"Action": "fail", "Test": "TestA"}, {"Action": "pass", "Test": "TestB"}]
        regressions = ratchet.check(self.BASELINE, current, {"TestA": "известный дефект"})
        self.assertEqual(regressions, [])

    def test_allowlist_requires_reason_and_date(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "allowlist.txt"

            path.write_text("TestA\n", encoding="utf-8")
            with self.assertRaises(SystemExit):
                ratchet.load_allowlist(path, "2026-11-17")

            path.write_text("TestA | | 2026-12-01\n", encoding="utf-8")
            with self.assertRaises(SystemExit):
                ratchet.load_allowlist(path, "2026-11-17")

            path.write_text("TestA | причина | 2026-01-01\n", encoding="utf-8")
            with self.assertRaises(SystemExit):
                ratchet.load_allowlist(path, "2026-11-17")

            path.write_text("# комментарий\nTestA | причина | 2026-12-31\n", encoding="utf-8")
            self.assertEqual(ratchet.load_allowlist(path, "2026-11-17"), {"TestA": "причина"})


class PassRateTest(unittest.TestCase):
    def test_groups(self):
        records = [
            {"Action": "pass", "Test": "TestBasicRoomCreation"},
            {"Action": "fail", "Test": "TestSync"},
            {"Action": "pass", "Test": "TestSend"},
        ]
        out = io.StringIO()
        ratchet.print_pass_rate(records, out)
        text = out.getvalue()
        self.assertIn("TestBasicRoomCreation: 1/1", text)
        self.assertIn("TestSync: 0/1", text)


class CliTest(unittest.TestCase):
    def test_empty_baseline_is_fail_closed(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            results = tmp_path / "results.gojson"
            baseline = tmp_path / "results.jsonl"
            allowlist = tmp_path / "allowlist.txt"

            results.write_text("\n".join(events({"Action": "pass", "Test": "TestA"})), encoding="utf-8")

            # Без baseline check обязан падать: иначе job зелёный, а проверки нет.
            self.assertEqual(
                ratchet.main(["check", "--input", str(results), "--baseline", str(baseline),
                              "--allowlist", str(allowlist)]),
                1,
            )

            # Только явный --update фиксирует планку.
            self.assertEqual(
                ratchet.main(["check", "--input", str(results), "--baseline", str(baseline),
                              "--allowlist", str(allowlist), "--update"]),
                0,
            )
            self.assertTrue(baseline.exists())

    def test_update_writes_baseline_and_check_passes(self):
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = pathlib.Path(tmp)
            results = tmp_path / "results.gojson"
            baseline = tmp_path / "results.jsonl"
            allowlist = tmp_path / "allowlist.txt"

            results.write_text("\n".join(events(
                {"Action": "pass", "Test": "TestA"},
                {"Action": "fail", "Test": "TestB"},
            )), encoding="utf-8")

            self.assertEqual(
                ratchet.main(["check", "--input", str(results), "--baseline", str(baseline),
                              "--allowlist", str(allowlist), "--update"]),
                0,
            )
            self.assertTrue(baseline.exists())

            # Тот же прогон — регрессий нет.
            self.assertEqual(
                ratchet.main(["check", "--input", str(results), "--baseline", str(baseline),
                              "--allowlist", str(allowlist)]),
                0,
            )

            # Искусственная поломка: тест, который проходил, теперь падает.
            results.write_text("\n".join(events({"Action": "fail", "Test": "TestA"})), encoding="utf-8")
            self.assertEqual(
                ratchet.main(["check", "--input", str(results), "--baseline", str(baseline),
                              "--allowlist", str(allowlist)]),
                1,
            )


class GuardTest(unittest.TestCase):
    """Известные обходы ratchet: каждый должен ловиться."""

    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.baseline = self.tmp / "results.jsonl"
        self.allowlist = self.tmp / "allowlist.txt"
        self.results = self.tmp / "run.gojson"
        ratchet.write_jsonl(self.baseline, [
            {"Action": "pass", "Test": "TestA"},
            {"Action": "pass", "Test": "TestB"},
            {"Action": "fail", "Test": "TestC"},
        ])

    def run_check(self, *records, update=False, today=None) -> int:
        self.results.write_text("\n".join(events(*records)), encoding="utf-8")
        argv = ["check", "--input", str(self.results), "--baseline", str(self.baseline),
                "--allowlist", str(self.allowlist)]
        if update:
            argv.append("--update")
        if today:
            argv += ["--today", today]

        return ratchet.main(argv)

    def test_normalize_cannot_overwrite_baseline(self):
        self.results.write_text("\n".join(events({"Action": "pass", "Test": "TestA"})), encoding="utf-8")
        before = self.baseline.read_text(encoding="utf-8")

        rc = ratchet.main(["normalize", "--input", str(self.results), "--baseline", str(self.baseline),
                           "--output", str(self.baseline)])
        self.assertEqual(rc, 2)
        self.assertEqual(ratchet.main(["normalize", "--input", str(self.results),
                                       "--baseline", str(self.baseline)]), 2)
        self.assertEqual(self.baseline.read_text(encoding="utf-8"), before)

    def test_failing_test_dropped_from_run_is_shrink(self):
        # TestC падал и пропал из прогона: pass не упал, но набор тестов сократился.
        rc = self.run_check({"Action": "pass", "Test": "TestA"}, {"Action": "pass", "Test": "TestB"})
        self.assertEqual(rc, 1)

    def test_update_refuses_regression_and_keeps_baseline(self):
        before = self.baseline.read_text(encoding="utf-8")
        rc = self.run_check({"Action": "pass", "Test": "TestA"}, {"Action": "fail", "Test": "TestB"},
                            {"Action": "fail", "Test": "TestC"}, update=True)
        self.assertEqual(rc, 1)
        self.assertEqual(self.baseline.read_text(encoding="utf-8"), before)

    def test_update_grows_baseline_with_header(self):
        rc = self.run_check({"Action": "pass", "Test": "TestA"}, {"Action": "pass", "Test": "TestB"},
                            {"Action": "pass", "Test": "TestC"}, {"Action": "fail", "Test": "TestD"},
                            update=True, today="2026-11-20")
        self.assertEqual(rc, 0)
        self.assertEqual(ratchet.baseline_measured(self.baseline), datetime.date(2026, 11, 20))
        self.assertEqual(len(ratchet.load_jsonl(self.baseline)), 4)

    def test_expired_allowlist_fails_with_default_today(self):
        # Раньше --today был зашит "2026-11-17", и просроченная запись проходила.
        self.allowlist.write_text("TestB | известный дефект | 2000-01-01\n", encoding="utf-8")
        with self.assertRaises(SystemExit):
            self.run_check({"Action": "pass", "Test": "TestA"}, {"Action": "fail", "Test": "TestB"},
                           {"Action": "fail", "Test": "TestC"})

    def test_empty_run_fails(self):
        self.results.write_text("not json\n", encoding="utf-8")
        self.assertEqual(ratchet.main(["check", "--input", str(self.results), "--baseline", str(self.baseline),
                                       "--allowlist", str(self.allowlist), "--update"]), 1)


class BaselineAgeTest(unittest.TestCase):
    """Baseline старше max-age предупреждает: иначе закрытые дефекты числятся
    падениями, а реальные регрессии теряются на их фоне."""

    def _baseline(self, tmp: pathlib.Path, mtime: datetime.date) -> pathlib.Path:
        path = tmp / "results.jsonl"
        path.write_text('{"Action": "pass", "Test": "TestA"}\n', encoding="utf-8")
        stamp = datetime.datetime.combine(mtime, datetime.time(12, 0)).timestamp()
        os.utime(path, (stamp, stamp))

        return path

    def test_age_days(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self._baseline(pathlib.Path(tmp), datetime.date(2026, 11, 10))
            self.assertEqual(
                ratchet.baseline_age_days(path, datetime.date(2026, 11, 17)), 7)

    def test_age_none_for_missing(self):
        with tempfile.TemporaryDirectory() as tmp:
            missing = pathlib.Path(tmp) / "nope.jsonl"
            self.assertIsNone(ratchet.baseline_age_days(missing, datetime.date(2026, 11, 17)))

    def test_age_by_header(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp) / "results.jsonl"
            path.write_text("# measured: 2026-11-01 pass 1/1\n{\"Action\": \"pass\", \"Test\": \"TestA\"}\n",
                            encoding="utf-8")
            self.assertEqual(ratchet.baseline_age_days(path, datetime.date(2026, 11, 20)), 19)
            self.assertEqual(len(ratchet.load_jsonl(path)), 1)


if __name__ == "__main__":
    unittest.main()
