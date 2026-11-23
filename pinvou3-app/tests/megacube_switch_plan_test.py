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



class SwitchPlanOwnershipTests(unittest.TestCase):
    """Exercise the production JSON-RPC tool and GET fact validators, without IO."""
    def plan(self, managed=True, can_adopt=True, status="running", healthy=True,
             default=False, omit_managed=False):
        row = {"card": "external-card", "status": status, "healthy": healthy,
               "managed": managed, "can_adopt": can_adopt, "model_id": "wire-id", "port": 18081}
        if omit_managed:
            row.pop("managed")
        facts = {
            "/v1/status": {"service": "cmsd", "version": "0.1.0", "uptime_s": 1,
                           "active_model": "external-card" if default else None,
                           "models": [], "degraded": []},
            "/v1/models": {"models": [row], "active_model": "external-card" if default else None,
                           "store_bindings": {}},
            "/v1/capacity?card=external-card": {
                "memory": {"available_gib": 90},
                "estimate": {"required_gib": 4, "safety_margin_gib": 1, "available_gib": 90}},
        }
        import json
        def get(path):
            self.assertIn(path, facts)
            return 200, json.dumps(facts[path])
        with patch.object(server, "_http_get", side_effect=get), \
                patch.object(server, "_http_post") as post, \
                patch.object(server, "_remember_local_device"), \
                patch.object(server, "_send") as send:
            server._handle({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                            "params": {"name": "cube_plan_switch", "arguments": {"card": "external-card"}}})
            post.assert_not_called()
            message = send.call_args.args[0]
        self.assertEqual(message["id"], 1)
        return json.loads(message["result"]["content"][0]["text"])

    def assert_no_direct_selection(self, result):
        data = result["data"]
        self.assertNotIn("already_active", data)
        self.assertNotIn("already_running", data)
        self.assertNotIn("budget_ok", data)
        self.assertIs(data["selection_permitted"], False)
        self.assertNotIn("cube_set_default_model", result["next"])
        self.assertNotIn("cube_switch_model", result["next"])

    def test_owned_healthy_keeps_selection_only_plan(self):
        result = self.plan()
        self.assertIs(result["data"]["already_running"], True)
        self.assertIs(result["data"]["budget_ok"], True)
        self.assertIn("cube_set_default_model", result["next"])

    def test_owned_healthy_default_keeps_idempotent_plan(self):
        self.assertIs(self.plan(default=True)["data"]["already_active"], True)

    def test_adoptable_healthy_requires_explicit_enable_before_selection(self):
        result = self.plan(managed=False)
        self.assert_no_direct_selection(result)
        self.assertIs(result["data"]["ready"], True)
        self.assertEqual(result["data"]["management_status"], "adoption_required")
        self.assertIn("cube_start_model", result["next"])
        self.assertIn("用户明确授权", result["next"])

    def test_read_only_healthy_never_suggests_mutation(self):
        result = self.plan(managed=False, can_adopt=False)
        self.assert_no_direct_selection(result)
        self.assertEqual(result["data"]["management_status"], "read_only")
        self.assertNotIn("cube_start_model", result["next"])

    def test_unknown_adoption_is_not_adoptable_or_owned(self):
        result = self.plan(managed=False, can_adopt=None)
        self.assert_no_direct_selection(result)
        self.assertEqual(result["data"]["management_status"], "unconfirmed")
        self.assertIsNone(result["data"]["can_adopt"])
        self.assertNotIn("cube_start_model", result["next"])

    def test_unknown_ownership_is_rejected_by_production_fact_validator(self):
        for kwargs in ({"managed": None}, {"omit_managed": True}):
            with self.subTest(kwargs=kwargs):
                result = self.plan(**kwargs)
                self.assertEqual(result["data"]["error_code"], "CMSD_INVALID_RESPONSE")
                self.assertEqual(result["data"]["observation"], "unconfirmed")
                self.assertNotIn("cube_set_default_model", result["next"])

    def test_stopped_adoptable_keeps_normal_switch_admission(self):
        result = self.plan(managed=False, status="stopped", healthy=False)
        self.assertIs(result["data"]["budget_ok"], True)
        self.assertEqual(result["data"]["start"], "external-card")
        self.assertIn("cube_switch_model", result["next"])

    def test_unowned_default_does_not_claim_idempotent_selection(self):
        result = self.plan(managed=False, default=True)
        self.assert_no_direct_selection(result)

    def test_stopped_read_only_does_not_offer_executable_plan(self):
        result = self.plan(managed=False, can_adopt=False, status="stopped", healthy=False)
        self.assert_no_direct_selection(result)
        self.assertIs(result["data"]["ready"], False)
        self.assertEqual(result["data"]["management_status"], "read_only")
        self.assertNotIn("cube_start_model", result["next"])

    def test_stopped_unknown_adoption_remains_unconfirmed(self):
        result = self.plan(managed=False, can_adopt=None, status="stopped", healthy=False)
        self.assert_no_direct_selection(result)
        self.assertEqual(result["data"]["management_status"], "unconfirmed")
        self.assertNotIn("cube_start_model", result["next"])



class CurrentLogicalObservationTests(unittest.TestCase):
    """Current identity is an observation, independent of management permission."""
    logical = "store-model"
    runtime = "store-model--v2"

    def row(self, **changes):
        return {"card": self.runtime, "logical_card": self.logical,
                "version_id": "v2", "deployment_revision": "revision2",
                "model_id": "wire-model", "port": 18081,
                "status": "stopped", "healthy": False, "managed": False,
                "can_adopt": True, "deployed": True, "weights_complete": True,
                **changes}

    def invoke(self, tool, arguments=None, *, rows=None, bindings=None,
               active=None, store_available=True, missing_bindings=False):
        import copy
        import json
        models = {"models": [self.row()] if rows is None else rows,
                  "active_model": active, "selection_policy": "latest-installed-v1",
                  "store_bindings": {} if bindings is None else bindings}
        if missing_bindings:
            models.pop("store_bindings")
        original = copy.deepcopy(models)
        calls = []
        def get(path):
            calls.append(path)
            if path == "/v1/status":
                value = {"service": "cmsd", "version": "0.1.0", "uptime_s": 1,
                         "active_model": active, "models": [], "degraded": []}
            elif path == "/v1/models":
                value = models
            elif path.startswith("/v1/capacity?card="):
                value = {"memory": {"available_gib": 20}, "estimate": {
                    "required_gib": 4, "safety_margin_gib": 1, "available_gib": 20}}
            else:
                self.fail("Unexpected read: " + path)
            return 200, json.dumps(value)
        store = (200, json.dumps([{"id": self.logical, "name": "Store model",
                                  "engine_name": "sglang", "size_bytes": 1024}]).encode())
        if not store_available:
            store = (503, b"unavailable")
        with patch.object(server, "_http_get", side_effect=get), \
                patch.object(server, "_http_post", side_effect=AssertionError("preview must not mutate")) as post, \
                patch.object(server, "_remember_local_device"), \
                patch.object(server, "_fetch", return_value=store), \
                patch.object(server, "_send") as send:
            server._handle({"jsonrpc": "2.0", "id": 42, "method": "tools/call",
                            "params": {"name": tool, "arguments": arguments or {}}})
            post.assert_not_called()
            message = send.call_args.args[0]
        self.assertEqual(message["id"], 42)
        self.assertEqual(models, original, "fact resolution must not rewrite ownership or bindings")
        return json.loads(message["result"]["content"][0]["text"]), calls

    def plan(self, row=None, **kwargs):
        result, calls = self.invoke("cube_plan_switch", {"card": self.logical},
                                    rows=[self.row() if row is None else row], **kwargs)
        self.assertIsInstance(result.get("data"), dict, result)
        return result, calls

    def assert_closed(self, rows, bindings=None, missing_bindings=False):
        result, _ = self.invoke("cube_plan_switch", {"card": self.logical},
                                rows=rows, bindings=bindings, missing_bindings=missing_bindings)
        self.assertIsInstance(result.get("data"), dict, result)
        self.assertEqual(result["data"].get("error_code"), "MODEL_SELECTION_UNCONFIRMED")
        catalog, _ = self.invoke("cube_list_store_models", rows=rows, bindings=bindings,
                                 missing_bindings=missing_bindings)
        self.assertEqual(catalog["data"]["local_observation"], "unconfirmed")
        self.assertEqual([row["local_state"] for row in catalog["data"]["models"]], ["unknown"])
        self.assertNotIn("cube_install_model", catalog["next"])

    def test_unowned_current_merges_once_under_store_identity(self):
        result, _ = self.invoke("cube_list_store_models")
        self.assertEqual(result["data"]["local_observation"], "available")
        self.assertEqual(len(result["data"]["models"]), 1)
        row = result["data"]["models"][0]
        self.assertEqual((row["card_id"], row["runtime_card"], row["local_state"]),
                         (self.logical, self.runtime, "downloaded"))

    def test_unowned_current_stays_logical_when_store_catalog_is_offline(self):
        result, _ = self.invoke("cube_list_store_models", store_available=False)
        self.assertEqual(len(result["data"]["models"]), 1)
        self.assertEqual(result["data"]["models"][0]["card_id"], self.logical)
        self.assertEqual(result["data"]["models"][0]["local_state"], "downloaded")
        self.assertFalse(result["data"]["models"][0]["store_listed"])

    def test_stopped_adoptable_current_retains_normal_logical_switch_preview(self):
        result, calls = self.plan()
        self.assertTrue(result["data"]["budget_ok"])
        self.assertEqual(result["data"]["start"], self.logical)
        self.assertIn("/v1/capacity?card=" + self.runtime, calls)
        self.assertNotIn("/v1/capacity?card=" + self.logical, calls)
        self.assertIn("cube_switch_model", result["next"])

    def test_healthy_unowned_current_still_requires_explicit_adoption(self):
        result, _ = self.plan(self.row(status="running", healthy=True))
        self.assertEqual(result["data"]["management_status"], "adoption_required")
        self.assertFalse(result["data"]["selection_permitted"])
        self.assertIn("cube_start_model", result["next"])
        self.assertNotIn("cube_set_default_model", result["next"])
        self.assertNotIn("cube_switch_model", result["next"])

    def test_readonly_and_unknown_adoption_do_not_gain_actions(self):
        for status in ["stopped", "running"]:
            for adoption, expected in [(False, "read_only"), (None, "unconfirmed")]:
                with self.subTest(status=status, adoption=adoption):
                    result, _ = self.plan(self.row(status=status, healthy=status == "running", can_adopt=adoption))
                    self.assertEqual(result["data"]["management_status"], expected)
                    self.assertFalse(result["data"]["selection_permitted"])
                    for action in ["cube_start_model", "cube_switch_model", "cube_set_default_model"]:
                        self.assertNotIn(action, result["next"])

    def test_future_runtime_state_remains_unknown(self):
        result, _ = self.plan(self.row(status="future-state", healthy=True))
        self.assertEqual(result["data"]["readiness"], "unknown")
        self.assertNotIn("cube_switch_model", result["next"])
        catalog, _ = self.invoke("cube_list_store_models", rows=[self.row(status="future-state")])
        self.assertEqual(catalog["data"]["models"][0]["local_state"], "unknown")

    def test_unknown_weights_do_not_become_missing_or_complete(self):
        result, _ = self.invoke("cube_list_store_models", rows=[self.row(weights_complete=None)])
        self.assertEqual(len(result["data"]["models"]), 1)
        self.assertEqual(result["data"]["models"][0]["local_state"], "unknown")
        self.assertNotIn("cube_install_model", result["next"])

    def test_undeployed_current_retains_readonly_plan(self):
        result, _ = self.plan(self.row(deployed=False, can_adopt=False, weights_complete=False))
        self.assertEqual(result["data"]["management_status"], "read_only")
        self.assertFalse(result["data"]["selection_permitted"])
        self.assertNotIn("cube_switch_model", result["next"])

    def test_managed_binding_remains_validated_and_routes_to_same_runtime(self):
        row = self.row(managed=True, status="running", healthy=True)
        result, calls = self.plan(row, bindings={self.logical: self.runtime})
        self.assertTrue(result["data"]["already_running"])
        self.assertIn("/v1/capacity?card=" + self.runtime, calls)
        self.assertIn("cube_set_default_model", result["next"])
        catalog, _ = self.invoke("cube_list_store_models", rows=[row], bindings={self.logical: self.runtime})
        self.assertEqual(len(catalog["data"]["models"]), 1)
        self.assertEqual(catalog["data"]["models"][0]["local_state"], "running")
        selected, _ = self.plan(row, bindings={self.logical: self.runtime}, active=self.runtime)
        self.assertTrue(selected["data"]["already_active"])

    def test_unowned_binding_is_still_invalid(self):
        self.assert_closed([self.row()], bindings={self.logical: self.runtime})

    def test_duplicate_current_logical_identity_is_rejected(self):
        self.assert_closed([self.row(), self.row(card="store-model--v3", version_id="v3")])

    def test_logical_alias_cannot_shadow_another_physical_row(self):
        self.assert_closed([self.row(), self.row(card=self.logical, logical_card=None)])

    def test_conflicting_bound_runtime_and_current_metadata_are_rejected(self):
        self.assert_closed([self.row(managed=True), self.row(card="store-model--v3")],
                           bindings={self.logical: self.runtime})

    def test_conflicting_logical_identity_for_one_bound_runtime_is_rejected(self):
        self.assert_closed([self.row(managed=True, logical_card="another-logical")],
                           bindings={self.logical: self.runtime})

    def test_invalid_current_logical_identity_is_rejected(self):
        for logical in ["", "a/b", "../model", ".", "x" * 129]:
            with self.subTest(logical=logical):
                self.assert_closed([self.row(logical_card=logical)])

    def test_missing_required_bindings_map_is_still_unconfirmed(self):
        self.assert_closed([self.row()], missing_bindings=True)

    def test_legacy_same_physical_id_and_external_rows_preserve_existing_behavior(self):
        result, _ = self.invoke("cube_list_store_models", rows=[self.row(card=self.logical, logical_card=self.logical)])
        self.assertEqual(len(result["data"]["models"]), 1)
        self.assertEqual(result["data"]["models"][0]["local_state"], "downloaded")
        result, _ = self.invoke("cube_list_store_models", rows=[self.row(card="external", logical_card=None)])
        self.assertEqual([(r["card_id"], r["local_state"]) for r in result["data"]["models"]],
                         [(self.logical, "none"), ("external", "downloaded")])


if __name__ == "__main__":
    unittest.main()
