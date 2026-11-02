"""Regression checks for required real-PDF test coverage."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("run_pdf_gate", Path(__file__).with_name("run_pdf_gate.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)
runner_spec = importlib.util.spec_from_file_location("run_pdf_unittest", Path(__file__).with_name("run_pdf_unittest.py"))
runner = importlib.util.module_from_spec(runner_spec)
runner_spec.loader.exec_module(runner)


def events():
    names = [gate.ROOT_TEST] + [gate.ROOT_TEST + "/" + name for name in gate.CASES]
    return [{"Action": action, "Test": name} for action in ("run", "pass") for name in names] + [{"Action": "pass"}]


def encode(items):
    return "\n".join(json.dumps(item) for item in items)


class GateTests(unittest.TestCase):
    def test_go_uses_venv_interpreter_without_resolving_symlinks(self):
        # POSIX venv/bin/python is often a symlink: resolving it can bypass
        # pyvenv.cfg and lose packages installed inside the venv.
        with tempfile.TemporaryDirectory() as directory:
            python = str(Path(directory) / "venv" / "bin" / "python")
            captured = {}

            def run(args, **kwargs):
                if args[1:3] == ["test", "-json"]:
                    captured.update(kwargs["env"])
                    return types.SimpleNamespace(returncode=0, stdout=encode(events()), stderr="")
                if any(str(arg).endswith("run_pdf_unittest.py") for arg in args):
                    return types.SimpleNamespace(returncode=0, stdout=json.dumps({
                        "tests": 1, "skipped": 0, "failures": 0, "errors": 0,
                        "expected_failures": 0, "unexpected_successes": 0,
                    }))
                return types.SimpleNamespace(returncode=0)

            with patch.dict(os.environ, {"EVIDENCE_PDF_TEST_PYTHON": python}), \
                 patch.object(gate.shutil, "which", side_effect=lambda value: value), \
                 patch.object(gate.subprocess, "run", side_effect=run), \
                 patch.object(gate.Path, "resolve", return_value=Path(directory) / "system-python"), \
                 patch.object(sys, "argv", ["gate"]), contextlib.redirect_stdout(io.StringIO()):
                gate.main()
            self.assertEqual(captured["EVIDENCE_PDF_TEST_PYTHON"], python)

    def test_python_suite_counts_and_failures(self):
        class Good(unittest.TestCase):
            def test_ok(self): pass
        class Skipped(unittest.TestCase):
            @unittest.skip("test")
            def test_skip(self): pass
        class Failed(unittest.TestCase):
            def test_fail(self): self.fail("synthetic")
        class Broken(unittest.TestCase):
            def test_error(self): raise RuntimeError("synthetic")
        class ExpectedFailure(unittest.TestCase):
            @unittest.expectedFailure
            def test_expected_failure(self): self.fail("synthetic")
        class UnexpectedSuccess(unittest.TestCase):
            @unittest.expectedFailure
            def test_unexpected_success(self): pass
        good = runner.run_suite(types.SimpleNamespace(Good=Good))
        self.assertEqual(gate.check_python_result(json.dumps(good)), 1)
        for module in (types.SimpleNamespace(), types.SimpleNamespace(Skipped=Skipped),
                       types.SimpleNamespace(Failed=Failed), types.SimpleNamespace(Broken=Broken),
                       types.SimpleNamespace(ExpectedFailure=ExpectedFailure),
                       types.SimpleNamespace(UnexpectedSuccess=UnexpectedSuccess)):
            with self.assertRaises(ValueError):
                gate.check_python_result(json.dumps(runner.run_suite(module)))

    def test_all_required_results_and_negative_controls(self):
        good = events()
        gate.check_results(encode(good))
        for invalid in ([], good[:-1], good + [{"Action": "skip", "Test": gate.ROOT_TEST}],
                        good + [{"Action": "fail", "Test": gate.ROOT_TEST}],
                        good + [good[0]], [x for x in good if x.get("Test") != gate.ROOT_TEST + "/plain-compressed.pdf"]):
            with self.subTest(size=len(invalid)), self.assertRaises(ValueError):
                gate.check_results(encode(invalid))

    def test_go_failure_summary_only_reports_allowlisted_events(self):
        sensitive = "synthetic-secret-and-path"
        output = encode([
            {"Action": "output", "Output": sensitive},
            {"Action": "fail", "Test": gate.ROOT_TEST + "/plain-compressed.pdf", "Output": sensitive},
            {"Action": "skip", "Test": gate.ROOT_TEST + "/" + sensitive},
            {"Action": "fail", "Test": [sensitive]},
        ]) + "\n" + sensitive
        summary = gate.go_failure_summary(output)
        self.assertIn("fail: " + gate.ROOT_TEST + "/plain-compressed.pdf", summary)
        self.assertNotIn(sensitive, summary)
        self.assertEqual(gate.go_failure_summary(sensitive),
                         "no classified failing test event; raw output withheld")

    def test_missing_runtime_fails_strict_but_is_optional_locally(self):
        result = types.SimpleNamespace(returncode=1)
        with patch.object(gate.subprocess, "run", return_value=result), patch.object(sys, "argv", ["gate", "--strict"]):
            with self.assertRaises(RuntimeError):
                gate.main()
        with patch.object(gate.subprocess, "run", return_value=result), patch.object(sys, "argv", ["gate"]), contextlib.redirect_stdout(io.StringIO()) as out:
            gate.main()
            self.assertIn("skipped", out.getvalue())

    def test_strict_rejects_missing_or_modified_fixture(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with patch.object(gate, "FIXTURES", root), patch.object(gate.subprocess, "run", return_value=types.SimpleNamespace(returncode=0)), patch.object(gate.shutil, "which", return_value="tool"), patch.object(sys, "argv", ["gate", "--strict"]):
                with self.assertRaises(FileNotFoundError):
                    gate.main()
                (root / "plain-compressed.pdf").write_bytes(b"modified")
                with self.assertRaises(ValueError):
                    gate.main()

    def test_strict_rejects_missing_audited_wheels(self):
        with patch.dict(os.environ, {"EVIDENCE_PDF_AUDIT_DIR": ""}), patch.object(gate.subprocess, "run", return_value=types.SimpleNamespace(returncode=0)), patch.object(gate.shutil, "which", return_value="tool"), patch.object(sys, "argv", ["gate", "--strict"]):
            with self.assertRaises(RuntimeError):
                gate.main()
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"EVIDENCE_PDF_AUDIT_DIR": directory, "EVIDENCE_PDF_REQUIRE_SYMLINK": "1"}):
            spec = importlib.util.spec_from_file_location("prune_test_missing_wheels", Path(__file__).with_name("test_prune_pdf_lxml.py"))
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            report = runner.run_suite(module)
            self.assertGreater(report["errors"] + report["failures"], 0)
            with self.assertRaises(ValueError):
                gate.check_python_result(json.dumps(report))


if __name__ == "__main__":
    unittest.main()
