"""Fail-closed result validation for the real closure PostgreSQL runner."""
import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import unittest
import tempfile
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    "account_closure_postgres_runner", Path(__file__).resolve().parents[2] / "scripts/test-account-closure-postgres.py")
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def stream(*events):
    rows = [{"Package": "fixture", **event} for event in events]
    rows.append({"Action": "pass", "Package": "fixture"})
    return "\n".join(json.dumps(event) for event in rows)


class ClosurePostgresResults(unittest.TestCase):
    def test_failed_child_diagnostics_are_visible_without_becoming_success(self):
        raw = stream({"Action": "output", "Test": "Root", "Output": "controlled failure detail\n"},
                     {"Action": "fail", "Test": "Root"})
        with tempfile.TemporaryDirectory() as directory:
            child_log = Path(directory) / "child.jsonl"
            child_log.write_text(raw, encoding="utf-8")
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                logs = runner.emit_child_logs(child_log, {"": {"Root"}, ".billing": {"Billing"}})
            self.assertIn("controlled failure detail", output.getvalue())
            self.assertNotIn(".billing", logs)
            with self.assertRaises(RuntimeError):
                runner.verify_results(logs[""], {"Root"})

    def test_counts_actual_parent_and_subcase_results(self):
        raw = stream({"Action": "pass", "Test": "Root/child"}, {"Action": "pass", "Test": "Root"})
        self.assertEqual(runner.verify_results(raw, {"Root"}), 2)

    def test_zero_tests_is_not_success(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Package": "fixture"}), {"Root"})

    def test_subcase_does_not_replace_required_root(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Root/child"}), {"Root"})

    def test_missing_required_child_is_not_success(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Owner"}), {"Owner", "Audit"})

    def test_skipped_subcase_fails_even_when_parent_passes(self):
        raw = stream({"Action": "skip", "Test": "Root/guard"}, {"Action": "pass", "Test": "Root"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_failed_package_is_not_hidden_by_passed_root(self):
        raw = stream({"Action": "pass", "Test": "Root"}, {"Action": "fail", "Package": "fixture"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_truncated_json_fails(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results('{"Action":', {"Root"})

    def test_empty_required_set_is_not_a_gate(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Root"}), set())

    def test_valid_json_prefix_without_package_completion_fails(self):
        raw = json.dumps({"Action": "pass", "Test": "Root", "Package": "fixture"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_unfinished_sibling_fails_even_if_root_and_package_pass(self):
        raw = stream({"Action": "pass", "Test": "Root"}, {"Action": "run", "Test": "Root/sibling"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_deployed_or_password_url_never_reaches_subprocess(self):
        for raw in ["postgres://user@db.example/postgres?sslmode=require",
                    "postgres://user:password@127.0.0.1:5433/postgres?sslmode=disable"]:
            with self.subTest(raw=raw), mock.patch.dict(os.environ, {"PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL": raw}), \
                    mock.patch.object(runner.subprocess, "run") as process:
                with self.assertRaises(ValueError):
                    runner.main()
                process.assert_not_called()


if __name__ == "__main__":
    unittest.main()
