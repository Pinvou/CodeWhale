import importlib.util
import json
from pathlib import Path
import runpy
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("deployment_contracts", ROOT / "scripts/account-platform-deployment-contracts.py")
deployment = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deployment)
verify_results = runpy.run_path(str(ROOT / "scripts/test-account-closure-postgres.py"))["verify_results"]


class DeploymentContracts(unittest.TestCase):
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
