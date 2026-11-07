import base64
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("financial_families", Path(__file__).resolve().parents[1] / "test-account-financial-families-postgres.py")
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class FakeInstance:
    def __init__(self):
        self.base = [["10", "postgres", True, True, True, True, True, True, True, -1]]
        self.base_members = [["3374", "3373", "10", False, True, True], ["3375", "3373", "10", False, True, True], ["3377", "3373", "10", False, True, True]]
        self.rows, self.members = copy.deepcopy(self.base), copy.deepcopy(self.base_members)
        self.created = self.dropped = 0
        self.clients, self.objects, self.dependencies = "0", "0", "f"
        self.extra_at_creation = False
        self.command_groups = []

    def __call__(self, sql):
        if isinstance(sql, tuple):
            self.command_groups.append(sql)
            sql = "\n".join(sql)
        if "CREATE ROLE" in sql:
            self.created += 1
            self.rows = copy.deepcopy(self.base) + [[str(20001+i), role, False, role == "billing_service", False, False, role != "billing_schema_owner", False, False, -1] for i, role in enumerate(sorted(RUNNER.FIN["ACTORS"]))]
            self.members = copy.deepcopy(self.base_members) + [["20002", "20001", "10", False, False, True], ["20003", "20001", "10", False, False, True], ["20003", "20002", "10", False, True, True]]
            if self.extra_at_creation:
                self.rows.append([999, "outside_actor", False, False, False, False, False, False, False, -1])
            return json.dumps({"roles": self.rows, "members": self.members})
        if "DROP ROLE" in sql:
            self.dropped += 1
            self.rows, self.members = copy.deepcopy(self.base), copy.deepcopy(self.base_members)
            return ""
        if sql == "SELECT current_user":
            return "postgres"
        if sql == RUNNER.DATABASE_FACTS:
            return "original-database-oids-and-acls"
        if sql == RUNNER.ROLE_FACTS:
            return json.dumps(self.rows)
        if sql == RUNNER.MEMBER_FACTS:
            return json.dumps(self.members)
        if "string_agg(datname" in sql:
            return "postgres,template0,template1"
        if "string_agg(rolname" in sql:
            return ",".join(sorted(row[1] for row in self.rows))
        if "session_user=current_user" in sql:
            return "true"
        if sql == "SHOW server_version_num":
            return "170011"
        if "pg_class" in sql:
            return self.objects
        if "pg_stat_activity" in sql:
            return self.clients
        if sql == RUNNER.EXTERNAL_BILLING_DEPENDENCIES:
            return self.dependencies
        raise AssertionError("unhandled fixture operation: " + sql)


class FinancialFamiliesTests(unittest.TestCase):
    def phase(self):
        return {"id": "original", "target": "./internal/billingschema", "package": RUNNER.TARGETS["./internal/billingschema"], "roots": {"TestParent": ["first"]}, "inputs": ["schema"], "mode": "owner", "children": [{"id": "f11", "stream_env": "F11_CHILD_LOG", "go_env": "F11_GO_BINARY", "package": RUNNER.TARGETS["./internal/billing"], "roots": {"TestChild": ["only"]}}]}

    def test_role_factory_preserves_baseline_and_retires_only_recorded_actors(self):
        query = FakeInstance()
        owner = RUNNER.OwnedInstance(query)
        owner.create_billing()
        setup, capture, commit = query.command_groups[0]
        self.assertTrue(setup.startswith("BEGIN;"))
        self.assertNotIn("COMMIT", setup)
        self.assertTrue(capture.startswith("SELECT jsonb_build_object("))
        self.assertNotIn("COMMIT", capture)
        self.assertEqual(commit, "COMMIT;")
        self.assertEqual(query.created, 1)
        self.assertIsNotNone(owner.retained_roles)
        owner.retire_billing()
        owner.retire_billing()
        self.assertEqual(query.dropped, 1)
        self.assertEqual(query.rows, query.base)
        self.assertEqual(query.members, query.base_members)

    def test_query_keeps_transaction_capture_in_separate_commands_on_one_backend(self):
        identity = ["postgres", "postgres", "postgres", "5432", "/declared/owned", "12345"]
        calls = []
        def run(argv, **options):
            calls.append(argv)
            return SimpleNamespace(returncode=0, stdout=json.dumps(identity) if len(calls) == 1 else '{"captured":true}', stderr="")
        with patch.dict(os.environ, {}, clear=True), patch.object(RUNNER.subprocess, "run", side_effect=run):
            query = RUNNER.query_function("owned-url")
            self.assertEqual(query(("BEGIN; CREATE ROLE controlled;", "SELECT 'capture';", "COMMIT;")), '{"captured":true}')
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[1][-6:], ["-c", "BEGIN; CREATE ROLE controlled;", "-c", "SELECT 'capture';", "-c", "COMMIT;"])
        self.assertIn("owned coordinator identity changed", calls[1][-7])
        self.assertIn("ON_ERROR_STOP=1", calls[1])

    def test_missing_capture_or_failed_commit_never_authorizes_role_cleanup(self):
        for mode in ("missing", "failed-commit"):
            query = FakeInstance()
            owner = RUNNER.OwnedInstance(query)
            def transaction(sql):
                result = query(sql)
                if isinstance(sql, tuple):
                    if mode == "failed-commit":
                        raise RuntimeError("owned family PostgreSQL operation failed")
                    return ""
                return result
            owner.query = transaction
            with self.subTest(mode=mode), self.assertRaises((RuntimeError, json.JSONDecodeError)):
                owner.create_billing()
            self.assertIsNone(owner.retained_roles)
            owner.retire_billing()
            self.assertEqual(query.dropped, 0)

    def test_dirty_admission_and_changed_resources_never_authorize_drop(self):
        for kind in ("clients", "objects", "role"):
            query = FakeInstance()
            if kind == "role":
                query.rows.append([999, "outside", False, False, False, False, False, False, False, -1])
            else:
                setattr(query, kind, "1")
            with self.subTest(admission=kind), self.assertRaises(RuntimeError):
                RUNNER.OwnedInstance(query)
            self.assertEqual((query.created, query.dropped), (0, 0))
        for kind in ("oid", "membership", "clients", "objects", "dependencies"):
            query = FakeInstance()
            owner = RUNNER.OwnedInstance(query)
            owner.create_billing()
            if kind == "oid":
                query.rows[-1][0] = str(int(query.rows[-1][0]) + 500)
            elif kind == "membership":
                query.members.append(["20001", "10", "10", False, True, True])
            else:
                setattr(query, kind, "t" if kind == "dependencies" else "1")
            with self.subTest(cleanup=kind), self.assertRaises(RuntimeError):
                owner.retire_billing()
            self.assertEqual(query.dropped, 0)

    def test_unrelated_actor_in_creation_snapshot_is_not_adopted(self):
        query = FakeInstance()
        owner = RUNNER.OwnedInstance(query)
        query.extra_at_creation = True
        with self.assertRaises(RuntimeError):
            owner.create_billing()
        owner.retire_billing()
        self.assertEqual(query.dropped, 0)

    def test_each_operation_keeps_identity_guard_on_its_own_backend(self):
        identity = ["postgres", "postgres", "postgres", "5432", "temporary/path'with-quote", "123456"]
        with patch.dict(os.environ, {}, clear=True), patch.object(RUNNER.subprocess, "run", side_effect=[SimpleNamespace(returncode=0, stdout=json.dumps(identity)), SimpleNamespace(returncode=0, stdout="observed\n")]) as run:
            query = RUNNER.query_function("postgres://postgres@127.0.0.1:49173/postgres?sslmode=disable")
            self.assertEqual(query("SELECT controlled_read"), "observed")
        argv = run.call_args.args[0]
        self.assertEqual(argv[-2:], ["-c", "SELECT controlled_read"])
        guard = argv[-3]
        self.assertIn("owned coordinator identity changed", guard)
        encoded = re.search(r"decode\('([A-Za-z0-9+/=]+)','base64'\)", guard)[1]
        self.assertEqual(json.loads(base64.b64decode(encoded)), identity)
        self.assertNotIn(identity[4], guard)
        self.assertIn("ON_ERROR_STOP=1", argv)

    def test_phase_inputs_child_logs_and_toolchain_are_bound(self):
        phase = self.phase()
        with patch.dict(os.environ, {"PINVOU_TEST_ACCOUNT_DATABASE_URL": "stale", "F11_CHILD_LOG": "stale", "F18_TEST_DATABASE_PROFILE": "stale", "GOFLAGS": "-trimpath"}, clear=True):
            env = RUNNER.phase_environment("owned-url", "/declared/go", phase, Path("/fresh/evidence"))
        self.assertNotIn("PINVOU_TEST_ACCOUNT_DATABASE_URL", env)
        self.assertNotIn("F18_TEST_DATABASE_PROFILE", env)
        self.assertEqual(env["PINVOU_TEST_SCHEMA_DATABASE_URL"], "owned-url")
        self.assertEqual(env["F11_CHILD_LOG"], str(Path("/fresh/evidence/f11.jsonl")))
        self.assertEqual(env["F11_GO_BINARY"], "/declared/go")
        self.assertEqual(env["GOFLAGS"], "-trimpath -race -tags=nomsgpack -mod=readonly")

    def stream(self, roots, package):
        events = [{"Action": "start", "Package": package}]
        for root, children in roots.items():
            events.append({"Action": "run", "Package": package, "Test": root})
            for child in children:
                for action in ("run", "pass"):
                    events.append({"Action": action, "Package": package, "Test": root + "/" + child})
            events.append({"Action": "pass", "Package": package, "Test": root})
        events.append({"Action": "pass", "Package": package})
        return "\n".join(json.dumps(event) for event in events) + "\n"

    def test_parent_success_requires_independent_complete_child_stream(self):
        phase = self.phase()
        for kind in ("valid", "missing", "wrong-package", "skip", "truncated", "parent-missing-case"):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temporary:
                def run(argv, **options):
                    self.assertTrue({"-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=20m"} <= set(argv))
                    self.assertEqual(argv[-1], phase["target"])
                    child = phase["children"][0]
                    stream = self.stream(child["roots"], child["package"] if kind != "wrong-package" else "another/package")
                    if kind == "skip":
                        stream = stream.replace('"Action": "pass"', '"Action": "skip"', 1)
                    if kind == "truncated":
                        stream = "\n".join(stream.splitlines()[:-1])
                    if kind != "missing":
                        Path(options["env"]["F11_CHILD_LOG"]).write_text(stream, encoding="utf-8")
                    roots = {"TestParent": []} if kind == "parent-missing-case" else phase["roots"]
                    return SimpleNamespace(returncode=0, stdout=self.stream(roots, phase["package"]), stderr="")
                with patch.object(RUNNER.subprocess, "run", side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                    if kind == "valid":
                        result = RUNNER.run_phase("owned-url", "/declared/go", phase, Path(temporary))
                        self.assertEqual(result["child_named_passes"], {"f11": 2})
                    else:
                        with self.assertRaises((RuntimeError, FileNotFoundError)):
                            RUNNER.run_phase("owned-url", "/declared/go", phase, Path(temporary))

    def test_real_manifest_keeps_complete_issue_scope_and_original_packages(self):
        manifest = RUNNER.load_manifest()
        self.assertEqual(len(manifest["issue_roots"]), 56)
        self.assertEqual(len(manifest["phases"]), 10)
        self.assertEqual(set(manifest["existing_roots"]), {"TestF06Review5566377593PublicPostgres", "TestMemberQuotaBatchPersistedFactsAndFinancialReplay", "TestF06MemberQuotaRestoreUsesOrdinaryPackageExpiry", "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration", "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration"})
        phases = {phase["id"]: phase for phase in manifest["phases"]}
        self.assertEqual(phases["f18-hold-recovery"]["mode"], "owner")
        self.assertEqual(phases["f18-hold-recovery"]["roots"], {
            "TestF18HoldRecoveryUsesTrustedFence": ["nil-proof", "unproven", "timeout", "controlled-trusted"],
            "TestF18OwnedRoleCleanupPostgres": ["normal", "partial-creation", "active-client", "external-membership", "replacement-role", "wrong-directory", "cancelled-context", "expired-context"],
        })
        self.assertEqual(set(phases["cross-domain-families"]["inputs"]), {"account", "billing"})
        self.assertEqual(phases["cross-domain-families"]["mode"], "retained")
        for child in phases["schema-families"]["children"]:
            self.assertEqual(child["package"], RUNNER.TARGETS["./internal/billing"])
        for phase in manifest["phases"]:
            if phase["id"].startswith("f09-"):
                self.assertEqual(phase["children"][0]["package"], "command-line-arguments")
        bonus = phases["billing-families"]["roots"]["TestRegistrationBonusLifetimeEligibilityPostgresIntegration"]
        self.assertEqual(set(bonus), {table + "/" + operation for table in ("registration_bonus_policy_versions", "registration_bonus_policy_commands", "registration_bonus_policy_audit", "registration_bonus_claims", "registration_bonus_receipts") for operation in ("UPDATE", "DELETE")})
        chargeback = phases["billing-families"]["roots"]["TestChargebackFrozenExecutionPostgres"]
        self.assertIn("actual-payment-origin-two-leg-reconciliation", chargeback)

    def test_f18_profiles_use_explicit_native_or_verified_ci_inputs(self):
        phase = next(item for item in RUNNER.load_manifest()["phases"] if item["id"] == "f18-hold-recovery")
        native = {"F18_TEST_DATABASE_PROFILE": "local-owned", "F18_TEST_DATABASE_USER": "owned", "F18_TEST_DATABASE_PORT": "49173", "F18_TEST_DATA_DIRECTORY": "declared-owned-directory"}
        with patch.dict(os.environ, native, clear=True):
            env = RUNNER.phase_environment("owned-url", "/declared/go", phase, Path("unused"))
            for key, value in native.items():
                self.assertEqual(env[key], value)
        ci = {**native, "GITHUB_ACTIONS": "true", "PINVOU_ACCOUNT_CI_PG_PORT": "49173", "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID": "1234567", "PINVOU_ACCOUNT_CI_PG_DIRECTORY": "/var/lib/postgresql/data"}
        with patch.dict(os.environ, ci, clear=True):
            env = RUNNER.phase_environment("owned-url", "/declared/go", phase, Path("unused"))
            self.assertEqual(env["F18_TEST_DATABASE_PROFILE"], "ci-owned")
            self.assertFalse(set(native).difference({"F18_TEST_DATABASE_PROFILE"}).intersection(env))
            for key, value in ci.items():
                if key.startswith("PINVOU_ACCOUNT_CI_PG_"):
                    self.assertEqual(env[key], value)
        for profile in ("", "unknown"):
            with patch.dict(os.environ, {"F18_TEST_DATABASE_PROFILE": profile}, clear=True), self.assertRaises(ValueError):
                RUNNER.phase_environment("owned-url", "/declared/go", phase, Path("unused"))

    def test_manifest_omission_duplicate_and_actor_order_fail_before_execution(self):
        manifest = RUNNER.load_manifest()
        changes = []
        missing = copy.deepcopy(manifest)
        missing["phases"].pop()
        changes.append(missing)
        duplicate = copy.deepcopy(manifest)
        duplicate["phases"].append(copy.deepcopy(duplicate["phases"][0]))
        changes.append(duplicate)
        bad_layout = copy.deepcopy(manifest)
        next(phase for phase in bad_layout["phases"] if phase["mode"] == "retained")["inputs"] = ["account"]
        changes.append(bad_layout)
        order = copy.deepcopy(manifest)
        order["phases"].reverse()
        changes.append(order)
        for value in changes:
            with self.subTest(value=value["phases"][0]["id"]), tempfile.TemporaryDirectory() as temporary:
                path = Path(temporary) / "manifest.json"
                path.write_text(json.dumps(value), encoding="utf-8")
                with self.assertRaises(ValueError):
                    RUNNER.load_manifest(path)


if __name__ == "__main__":
    unittest.main()
