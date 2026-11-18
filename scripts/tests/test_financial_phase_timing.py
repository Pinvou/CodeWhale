import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("financial_timing", Path(__file__).resolve().parents[1] / "test-token-financial-postgres.py")
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class FinancialTimingTests(unittest.TestCase):
    def test_stream_preserves_failure_and_json_evidence(self):
        command = [sys.executable, "-c", "import sys;print('{\"Action\":\"run\",\"Test\":\"Root\"}',flush=True);print('compiler detail',file=sys.stderr,flush=True);sys.exit(7)"]
        with contextlib.redirect_stdout(io.StringIO()) as output:
            result = RUNNER.run_go_phase(command, env=os.environ.copy())
        self.assertEqual(result.returncode, 7)
        self.assertIn("compiler detail", output.getvalue())
        self.assertEqual(json.loads(result.stdout), {"Action": "run", "Test": "Root"})

    def test_receipt_reports_root_cost_without_counting_children_twice(self):
        events = [{"Action": "pass", "Test": "Root", "Elapsed": 12},
                  {"Action": "pass", "Test": "Root/child", "Elapsed": 11},
                  {"Action": "skip", "Test": "NotExecuted"},
                  {"Action": "start", "Time": "1970-01-01T00:01:42.123456789Z"},
                  {"Action": "pass", "Elapsed": 12},
                  {"Action": "output", "Output": '[ci-fixture-timing] {"operation":"template-clone","seconds":0.2}\n'},
                  {"Action": "output", "Output": '[ci-fixture-template] {"prepared":1,"reused":3,"remaining":0}\n'}]
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"PINVOU_CI_RESOURCE_LEDGER": directory}, clear=True), contextlib.redirect_stdout(io.StringIO()):
            RUNNER.phase_timing("owner-schema", 13, events, 7, started_epoch=100)
            receipt = json.loads((Path(directory) / "financial-phase-owner-schema.json").read_text())
        self.assertEqual(receipt["root_passes"], ["Root"])
        self.assertEqual(receipt["slow_roots"], [{"test": "Root", "seconds": 12}])
        self.assertEqual(receipt["exit_code"], 7)
        self.assertAlmostEqual(receipt["pre_test_seconds"], 2.123456, places=6)
        self.assertEqual(receipt["package_elapsed_seconds"], 12)
        self.assertEqual(receipt["fixture_operations"]["template-clone"], {"count": 1, "seconds": 0.2})
        self.assertEqual(receipt["templates"], {"prepared": 1, "reused": 3, "remaining": 0})

    def test_template_probe_cannot_skip_or_omit_one_safety_case(self):
        root = "TestDatabaseTemplateIsolationPostgres"
        package = "github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
        events = [{"Action": "run", "Test": root, "Package": package}]
        for child in RUNNER.EXACT_REQUIRED_CHILDREN[root]:
            events.append({"Action": "pass", "Test": root + "/" + child, "Package": package})
        events += [{"Action": "pass", "Test": root, "Package": package}, {"Action": "pass", "Package": package}]
        options = {"package": package, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in RUNNER.EXACT_REQUIRED_CHILDREN[root]:
            with self.subTest(child=child), self.assertRaises(RuntimeError):
                RUNNER.verify_results([e for e in events if e.get("Test") != root + "/" + child], {root}, **options)


if __name__ == "__main__":
    unittest.main()
