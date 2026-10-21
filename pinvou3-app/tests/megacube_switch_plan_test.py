"""A default pointer and an unknown health result are not readiness evidence."""
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch


SERVER = Path(__file__).resolve().parents[1] / "src-tauri/resources/common/bundle/mcp-servers/megacube_admin_server.py"
SPEC = importlib.util.spec_from_file_location("switch_plan_server", SERVER)
server = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(server)

MISSING = object()


class SwitchPlanReadinessTests(unittest.TestCase):
    def plan(self, status, healthy, default=False):
        row = {"card": "model.v1", "status": status, "managed": True, "port": 18081}
        if healthy is not MISSING:
            row["healthy"] = healthy
        calls = []

        def query(path):
            calls.append(path)
            if path == "/v1/status":
                return {"service": "cmsd", "active_model": "model.v1" if default else "other"}
            if path == "/v1/models":
                return {"selection_policy": "latest-installed-v1", "store_bindings": {"model": "model.v1"},
                        "models": [row]}
            if path == "/v1/capacity?card=model.v1":
                return {"estimate": {"required_gib": 4, "safety_margin_gib": 1, "available_gib": 20}}
            self.fail("Unexpected observation: " + path)

        with patch.object(server, "_api", side_effect=query), patch.object(server, "_api_post") as post:
            result = server._tool_plan_switch({"card": "model"})
            post.assert_not_called()
        self.assertIn("/v1/capacity?card=model.v1", calls)
        return result

    def assert_unready(self, result, reason, default):
        data = result["data"]
        self.assertFalse(data.get("already_active", False))
        self.assertFalse(data.get("already_running", False))
        self.assertFalse(data.get("budget_ok", False))
        self.assertIs(data["ready"], False)
        self.assertIs(data["default_selected"], default)
        self.assertEqual(data["readiness"], reason)
        self.assertIn("尚未确认可用", result["summary"])
        self.assertIn("cube_get_logs", result["next"])
        self.assertNotIn("cube_set_default_model", result["next"])
        self.assertNotIn("无需任何操作", result["next"])

    def test_healthy_default_remains_idempotent(self):
        result = self.plan("running", True, default=True)
        self.assertIs(result["data"]["already_active"], True)
        self.assertNotIn("already_running", result["data"])

    def test_unhealthy_default_requires_diagnosis(self):
        self.assert_unready(self.plan("running", False, default=True), "unhealthy", True)

    def test_stopped_default_is_not_a_usable_service(self):
        for healthy in (False, None, MISSING):
            with self.subTest(healthy=healthy):
                self.assert_unready(self.plan("stopped", healthy, default=True), "stopped", True)

    def test_default_health_must_be_positively_confirmed(self):
        for healthy in (None, MISSING):
            with self.subTest(healthy=healthy):
                self.assert_unready(self.plan("running", healthy, default=True), "unknown", True)

    def test_healthy_nondefault_permits_selection_only(self):
        result = self.plan("running", True)
        self.assertIs(result["data"]["already_running"], True)
        self.assertIs(result["data"]["budget_ok"], True)
        self.assertIn("cube_set_default_model", result["next"])

    def test_unhealthy_nondefault_does_not_suggest_selection(self):
        self.assert_unready(self.plan("running", False), "unhealthy", False)

    def test_nondefault_unknown_health_does_not_suggest_selection(self):
        for healthy in (None, MISSING):
            with self.subTest(healthy=healthy):
                self.assert_unready(self.plan("running", healthy), "unknown", False)

    def test_degraded_and_starting_observe_before_recovery(self):
        for status in ("degraded", "starting"):
            for default in (True, False):
                with self.subTest(status=status, default=default):
                    self.assert_unready(self.plan(status, False, default=default), status, default)

    def test_unknown_runtime_state_does_not_produce_a_plan(self):
        for status in (None, "unknown"):
            with self.subTest(status=status):
                self.assert_unready(self.plan(status, True), "unknown", False)

    def test_stopped_nondefault_retains_normal_budget_plan(self):
        for healthy in (False, None, MISSING):
            with self.subTest(healthy=healthy):
                result = self.plan("stopped", healthy)
                self.assertIs(result["data"]["budget_ok"], True)
                self.assertEqual(result["data"]["start"], "model")
                self.assertIn("cube_switch_model", result["next"])
                self.assertNotIn("already_running", result["data"])


if __name__ == "__main__":
    unittest.main()
