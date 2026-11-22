import contextlib
import importlib.util
import io
from pathlib import Path
import subprocess
import unittest
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "account-platform-openbao-fixture.py"
SPEC = importlib.util.spec_from_file_location("openbao_fixture_safety", SCRIPT)
MOD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MOD)


class OpenBaoFixtureSafetyTests(unittest.TestCase):
    def test_docker_failure_never_publishes_unlabelled_secret_output(self):
        canary = "unlabelled-private-value-444444444444"
        output = io.StringIO()
        result = subprocess.CompletedProcess(["docker", "inspect"], 1, canary, canary)
        with mock.patch.object(MOD.subprocess, "run", return_value=result), contextlib.redirect_stdout(output):
            with self.assertRaisesRegex(RuntimeError, "^FIXTURE_DOCKER_OPERATION_FAILED$"):
                MOD.docker("inspect", "owned-fixture")
        self.assertEqual(output.getvalue(), "FIXTURE_DOCKER_OPERATION_FAILED\n")
        self.assertNotIn(canary, output.getvalue())

    def test_current_help_excludes_uninstalled_native_gate(self):
        output = io.StringIO()
        with mock.patch("sys.argv", [str(SCRIPT), "--help"]), contextlib.redirect_stdout(output):
            with self.assertRaises(SystemExit) as raised:
                MOD.main()
        self.assertEqual(raised.exception.code, 0)
        self.assertNotIn("native-gate-runner", output.getvalue())
        self.assertIn("--operator-test-binary", output.getvalue())
        self.assertIn("--transit-test-binary", output.getvalue())


if __name__ == "__main__":
    unittest.main()
