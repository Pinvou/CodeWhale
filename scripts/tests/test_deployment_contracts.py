import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import runpy
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("deployment_contracts", ROOT / "scripts/account-platform-deployment-contracts.py")
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)
verify_results = runpy.run_path(str(ROOT / "scripts/test-account-closure-postgres.py"))["verify_results"]


class DeploymentContracts(unittest.TestCase):
    def test_dependency_failure_retains_exit_and_private_log_with_bounded_redacted_diagnostics(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "dependencies.log"
            errors = io.StringIO()
            secret = "fixture-private-token-not-public"
            program = "import sys; print('x'*70000); print('registry timeout https://user:password@example.invalid/package?token=private'); print('Authorization: Bearer fixture-header-secret'); print('npm _authToken=fixture-config-secret'); print('env '+sys.argv[1]); sys.exit(7)"
            with patch.dict(os.environ, {"NPM_TOKEN": secret}), contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run") as raised:
                    deployment.prepare_dependencies([sys.executable, "-c", program, secret], logfile)
            emitted = errors.getvalue()
            self.assertIn("DEPENDENCY_PREPARATION_FAILED exit=7 log=dependencies.log", emitted)
            self.assertIn("registry timeout", emitted)
            self.assertLessEqual(len(emitted), 24200)
            for value in (secret, "user:password", "token=private", "fixture-header-secret", "fixture-config-secret"):
                self.assertNotIn(value, emitted)
                self.assertNotIn(value, str(raised.exception))
            self.assertIn(secret, logfile.read_text())
            self.assertGreater(logfile.stat().st_size, 65536)

    def test_successful_dependency_preparation_is_not_a_contract_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            logfile = Path(directory) / "dependencies.log"
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                deployment.prepare_dependencies([sys.executable, "-c", "print('dependencies ready')"], logfile)
            self.assertEqual(errors.getvalue(), "")
            self.assertEqual(logfile.read_text().strip(), "dependencies ready")

    def test_truncated_url_cannot_expose_query_credentials_after_its_prefix_is_lost(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = "controlled-fixture-query-credential"
            payload = root / "payload.txt"
            payload.write_text("https://example.invalid/" + "x" * 70000 + "?auth=" + value + "\n")
            script = root / "emit.py"
            script.write_text("from pathlib import Path\nimport sys\nsys.stdout.write(Path(sys.argv[1]).read_text())\nsys.exit(7)\n")
            logfile = root / "dependencies.log"
            errors = io.StringIO()
            with contextlib.redirect_stderr(errors):
                with self.assertRaisesRegex(RuntimeError, "mandatory contracts did not run"):
                    deployment.prepare_dependencies([sys.executable, str(script), str(payload)], logfile)
            self.assertNotIn(value, errors.getvalue())
            self.assertIn("DEPENDENCY_PREPARATION_FAILED exit=7", errors.getvalue())
            self.assertIn(value, logfile.read_text())

    def test_published_fixture_is_complete_and_uses_frozen_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            deployment.published_history(ROOT, target)
            self.assertEqual(len(list((target / "account").glob("*.up.sql"))), 87)
            self.assertEqual(len(list((target / "token").glob("*.up.sql"))), 34)
            self.assertNotIn(b"publication_generation", (target / "account/000003_event_delivery.up.sql").read_bytes())
            self.assertIn(b"event_replay_requests_immutable", (target / "token/000018_event_replay_audit.up.sql").read_bytes())

    def test_required_root_cannot_hide_skipped_child_or_incomplete_stream(self):
        def events(*values):
            return "\n".join(json.dumps(value) for value in values)
        start = {"Action": "start", "Package": "fixture/package"}
        parent = {"Action": "pass", "Package": "fixture/package", "Test": "Required"}
        end = {"Action": "pass", "Package": "fixture/package"}
        for raw in (
            events(start, parent, {"Action": "skip", "Package": "fixture/package", "Test": "Required/actual-database"}, end),
            events(start, parent),
            events(start, {"Action": "pass", "Package": "fixture/package", "Test": "Unrelated"}, end),
        ):
            with self.assertRaises(RuntimeError):
                verify_results(raw, {"Required"})

    def test_published_history_rejects_missing_or_escaping_entries(self):
        original = json.loads((ROOT / "account-platform/test/fixtures/production-history-manifest.json").read_text())
        for mutate in (lambda value: value["domains"]["account"].pop(),
                       lambda value: value["domains"]["account"][0].update(file="../outside.up.sql")):
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = root / "account-platform/test/fixtures/production-history-manifest.json"
                path.parent.mkdir(parents=True)
                value = json.loads(json.dumps(original))
                mutate(value)
                path.write_text(json.dumps(value))
                with self.assertRaises(RuntimeError):
                    deployment.published_history(root, root / "legacy")


if __name__ == "__main__":
    unittest.main()
