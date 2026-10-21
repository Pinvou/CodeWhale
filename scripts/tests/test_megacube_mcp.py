"""Run MegaCube's real stdio/HTTP/socket fixtures in the existing Python CI gate."""
import os
from pathlib import Path
import subprocess
import sys
import unittest

ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(sys.platform == "linux", "MegaCube local runner targets Linux")
class MegaCubeMcpTests(unittest.TestCase):
    def run_suite(self, script):
        result = subprocess.run(
            [sys.executable, "-B", str(ROOT / "pinvou3-app/tests" / script)],
            cwd=ROOT, env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
            capture_output=True, text=True, timeout=120,
        )
        self.assertEqual(result.returncode, 0, (result.stdout + result.stderr)[-16000:])

    def test_standard_model_installation(self):
        self.run_suite("megacube_mcp_smoke.py")

    def test_atomic_acquisition_and_external_artifacts(self):
        self.run_suite("megacube_atomic_tools_test.py")

    def test_installation_workflow_and_local_reuse(self):
        self.run_suite("megacube_installation_workflow_test.py")

    def test_reviewed_install_retry(self):
        self.run_suite("megacube_install_retry_test.py")

    def test_local_inventory_contract(self):
        self.run_suite("megacube_inventory_contract_test.py")

    def test_independent_record_origin(self):
        self.run_suite("megacube_record_origin_test.py")

    def test_switch_plan_positive_readiness(self):
        self.run_suite("megacube_switch_plan_test.py")

    def test_runtime_stays_on_the_selected_local_endpoint(self):
        self.run_suite("megacube_runtime_boundary_test.py")

    def test_management_observations_are_verified(self):
        self.run_suite("megacube_api_observation_test.py")

    def test_generated_deployment_paths_are_literal(self):
        self.run_suite("megacube_generated_env_test.py")
