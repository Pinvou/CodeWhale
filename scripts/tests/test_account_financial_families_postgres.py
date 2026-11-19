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

    def __call__(self, sql, *, timeout=30):
        if isinstance(sql, tuple):
            self.command_groups.append(sql)
            sql = "\n".join(sql)
        if sql == RUNNER.RESTORATION_SNAPSHOT:
            return json.dumps({"databases": "original-database-oids-and-acls",
                               "roles": self.rows, "members": self.members,
                               "objects": int(self.objects), "clients": int(self.clients),
                               "external_billing_dependencies": self.dependencies == "t"})
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

    def test_family_child_cache_modes_do_not_inherit_or_mutate_the_parent(self):
        parent = {
            "GITHUB_ACTIONS": "true",
            "PINVOU_CI_FIXTURE_TEMPLATES": "1",
            "PINVOU_CI_RETAINED_BILLING_TEMPLATES": "1",
            "PINVOU_CI_OWNER_WALLET_TEMPLATES": "1",
            "PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL": "original-parent-url",
            "PINVOU_ACCOUNT_CI_PG_PORT": "49173",
            "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID": "1234567",
            "PINVOU_ACCOUNT_CI_PG_DIRECTORY": "/declared/owned",
        }
        with patch.dict(os.environ, parent, clear=True):
            before = dict(os.environ)
            for phase in RUNNER.load_manifest()["phases"]:
                with self.subTest(phase=phase["id"]):
                    child = RUNNER.phase_environment("owned-url", "/declared/go", phase, Path("/fresh/evidence"))
                    self.assertEqual(child["PINVOU_CI_FIXTURE_TEMPLATES"], "1")
                    for key in ("PINVOU_CI_RETAINED_BILLING_TEMPLATES", "PINVOU_CI_OWNER_WALLET_TEMPLATES"):
                        self.assertEqual(child[key], "0")
                    for key, value in parent.items():
                        if key.startswith("PINVOU_ACCOUNT_") or key == "GITHUB_ACTIONS":
                            self.assertEqual(child[key], value)
                    self.assertEqual(dict(os.environ), before)

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
            def transaction(sql, **options):
                result = query(sql, **options)
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
            clock = patch.object(RUNNER.time, "monotonic", side_effect=[0, 0, 15]) if kind == "clients" else contextlib.nullcontext()
            with self.subTest(admission=kind), clock, self.assertRaises(RuntimeError):
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
            clock = patch.object(RUNNER.time, "monotonic", side_effect=[0, 0, 15]) if kind == "clients" else contextlib.nullcontext()
            with self.subTest(cleanup=kind), clock, self.assertRaises(RuntimeError):
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

    def test_restoration_waits_for_real_snapshot_clients_without_resetting_budget(self):
        fixture = FakeInstance()
        owner = RUNNER.OwnedInstance(fixture)
        values = []
        for clients in (2, 1, 0):
            value = json.loads(fixture(RUNNER.RESTORATION_SNAPSHOT))
            value["clients"] = clients
            values.append(json.dumps(value))
        clock, calls, pauses = [0.0], [], []
        def query(sql, *, timeout):
            calls.append((sql, timeout))
            return values.pop(0)
        def pause(delay):
            pauses.append(delay)
            clock[0] += delay
        owner.query = query
        owner.assert_restored(now=lambda: clock[0], pause=pause)
        self.assertEqual([sql for sql, _ in calls], [RUNNER.RESTORATION_SNAPSHOT] * 3)
        self.assertEqual(pauses, [0.05, 0.05])
        self.assertGreater(calls[0][1], calls[1][1])
        self.assertGreater(calls[1][1], calls[2][1])
        self.assertEqual(owner.last_observation["clients"], "0")
        self.assertEqual(owner.last_observation["roles"], fixture.base)
        self.assertEqual((fixture.created, fixture.dropped), (0, 0))
        # An already clean snapshot needs exactly one query and no observation delay.
        calls.clear(); pauses.clear()
        owner.query = lambda sql, **options: (calls.append(sql) or fixture(sql, **options))
        owner.assert_restored(now=lambda: 0, pause=pauses.append)
        self.assertEqual(calls, [RUNNER.RESTORATION_SNAPSHOT])
        self.assertEqual(pauses, [])

    def test_restoration_deadline_and_late_zero_never_authorize_continuation(self):
        fixture = FakeInstance()
        owner = RUNNER.OwnedInstance(fixture)
        clock, calls = [0.0], []
        def active(sql, *, timeout):
            calls.append(timeout)
            value = json.loads(fixture(sql))
            value["clients"] = 1
            return json.dumps(value)
        owner.query = active
        with self.assertRaisesRegex(RuntimeError, "another client"):
            owner.assert_restored(timeout_seconds=0.12, now=lambda: clock[0],
                                  pause=lambda delay: clock.__setitem__(0, clock[0] + delay))
        self.assertEqual(len(calls), 3)
        self.assertTrue(all(0 < value <= 0.12 for value in calls))
        clock[0] = 0
        def late(sql, *, timeout):
            clock[0] = 15
            return fixture(sql)
        owner.query = late
        with self.assertRaisesRegex(RuntimeError, "another client"):
            owner.assert_restored(now=lambda: clock[0], pause=lambda _: self.fail("late result waited"))
        for budget in (0, -1, True, "15", 16, float("inf"), float("nan")):
            with self.subTest(budget=budget), self.assertRaises(ValueError):
                owner.assert_restored(timeout_seconds=budget)
        self.assertEqual((fixture.created, fixture.dropped), (0, 0))

    def test_restoration_drift_and_query_errors_are_not_waited_through(self):
        fixture = FakeInstance()
        owner = RUNNER.OwnedInstance(fixture)
        owner.create_billing()
        valid = json.loads(fixture(RUNNER.RESTORATION_SNAPSHOT))
        valid["clients"] = 1
        invalid = []
        for field, value in (("databases", "different-database-oid-owner-or-acl"),
                             ("objects", 1), ("objects", False), ("clients", -1),
                             ("clients", False), ("clients", "0"),
                             ("external_billing_dependencies", True)):
            invalid.append({**copy.deepcopy(valid), field: value})
        role_oid = copy.deepcopy(valid); role_oid["roles"][-1][0] = "99999"; invalid.append(role_oid)
        role_flag = copy.deepcopy(valid); role_flag["roles"][-1][2] = True; invalid.append(role_flag)
        membership = copy.deepcopy(valid); membership["members"][-1][2] = "99999"; invalid.append(membership)
        invalid.extend([{}, [], {**valid, "unknown": 1}])
        for bad in invalid:
            replies, calls, clock = [json.dumps(valid), json.dumps(bad)], [], [0.0]
            def query(sql, *, timeout):
                calls.append(sql)
                return replies.pop(0)
            owner.query = query
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                owner.assert_restored(now=lambda: clock[0], pause=lambda d: clock.__setitem__(0, clock[0] + d))
            self.assertEqual(calls, [RUNNER.RESTORATION_SNAPSHOT] * 2)
        for error in (RuntimeError("identity changed"), RUNNER.subprocess.TimeoutExpired("owned-psql", 1)):
            calls = []
            def broken(sql, *, timeout):
                calls.append(sql)
                raise error
            owner.query = broken
            with self.subTest(error=type(error)), self.assertRaises(RuntimeError):
                owner.assert_restored(pause=lambda _: self.fail("query error waited"))
            self.assertEqual(calls, [RUNNER.RESTORATION_SNAPSHOT])
        self.assertEqual((fixture.created, fixture.dropped), (1, 0))

    def test_observation_query_uses_remaining_timeout_and_ordinary_sql_keeps_30(self):
        identity = ["postgres", "postgres", "postgres", "5432", "/owned", "12345"]
        with patch.dict(os.environ, {}, clear=True), patch.object(RUNNER.subprocess, "run", side_effect=[
                SimpleNamespace(returncode=0, stdout=json.dumps(identity)),
                SimpleNamespace(returncode=0, stdout="snapshot"),
                SimpleNamespace(returncode=0, stdout="ddl")]) as run:
            query = RUNNER.query_function("owned-url")
            self.assertEqual(query(RUNNER.RESTORATION_SNAPSHOT, timeout=2.25), "snapshot")
            self.assertEqual(query(("BEGIN;", "SELECT 'capture';", "COMMIT;")), "ddl")
            self.assertEqual(run.call_args_list[1].kwargs["timeout"], 2.25)
            self.assertIn("owned coordinator identity changed", run.call_args_list[1].args[0][-3])
            self.assertEqual(run.call_args_list[1].args[0][-1], RUNNER.RESTORATION_SNAPSHOT)
            self.assertEqual(run.call_args_list[2].kwargs["timeout"], 30)
            self.assertEqual(run.call_args_list[2].args[0][-6:], ["-c", "BEGIN;", "-c", "SELECT 'capture';", "-c", "COMMIT;"])
            for timeout in (0, -1, True, 31, float("nan")):
                with self.subTest(timeout=timeout), self.assertRaises(ValueError):
                    query(RUNNER.RESTORATION_SNAPSHOT, timeout=timeout)
            self.assertEqual(run.call_count, 3)

    def test_failed_phase_is_not_replaced_by_successful_resource_restoration(self):
        fixture = FakeInstance()
        fixture.identity = ["postgres", "postgres", "postgres", "5432", "/owned", "12345"]
        failure = RuntimeError("original financial phase failed")
        with tempfile.TemporaryDirectory() as temporary:
            artifacts = Path(temporary) / "evidence"
            with patch.dict(os.environ, {
                    "PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL": "postgres://postgres@127.0.0.1:49173/postgres?sslmode=disable",
                    "PINVOU_FINANCIAL_CHILD_ARTIFACTS": str(artifacts)}, clear=True), \
                 patch.object(RUNNER, "load_manifest", return_value={"phases": [self.phase()]}), \
                 patch.object(RUNNER.shutil, "which", return_value="declared-go"), \
                 patch.object(RUNNER, "source_identity", return_value={"head": "fixed", "dirty": False}), \
                 patch.object(RUNNER, "query_function", return_value=fixture), \
                 patch.object(RUNNER, "run_phase", side_effect=failure), \
                 contextlib.redirect_stdout(io.StringIO()), self.assertRaises(RuntimeError) as caught:
                RUNNER.main()
            self.assertIs(caught.exception, failure)
            resources = json.loads((artifacts / "resources.json").read_text(encoding="utf-8"))
            self.assertEqual(resources["state"], "released")
            self.assertTrue(resources["phases"][0]["resources_confirmed"])
            self.assertFalse((artifacts / "summary.json").exists())
            self.assertEqual((fixture.created, fixture.dropped), (0, 0))

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
        self.assertEqual(len(manifest["issue_roots"]), 57)
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

    def test_chargeback_source_budget_requires_selection_and_all_outcomes(self):
        manifest = RUNNER.load_manifest()
        root = "TestChargebackFinancialRepairSourceBudgetPostgres"
        children = ["financial-repair", "confirmed-chargeback", "dismissed-no-effect"]
        self.assertIn(root, manifest["issue_roots"])
        phase = next(item for item in manifest["phases"] if item["id"] == "billing-families")
        self.assertEqual(phase["roots"][root], children)
        self.assertEqual(phase["target"], "./internal/billing")
        self.assertEqual(phase["mode"], "retained")
        self.assertEqual(phase["inputs"], ["billing"])
        stream = self.stream(phase["roots"], phase["package"])
        events = [json.loads(line) for line in stream.splitlines()]
        variants = [("valid", events)]
        for child in children:
            name = root + "/" + child
            variants.append(("missing-" + child, [event for event in events if event.get("Test") != name]))
            for action in ("skip", "fail"):
                variants.append((action + "-" + child, [
                    {**event, "Action": action} if event.get("Test") == name and event["Action"] == "pass" else event
                    for event in events
                ]))
        variants.append(("missing-root", [event for event in events if event.get("Test", "").split("/", 1)[0] != root]))
        for kind, candidate in variants:
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as temporary:
                def run(argv, **options):
                    self.assertIn(root, argv[argv.index("-run") + 1])
                    self.assertEqual(argv[-1], "./internal/billing")
                    return SimpleNamespace(returncode=0, stdout="\n".join(json.dumps(event) for event in candidate), stderr="")
                with patch.object(RUNNER.subprocess, "run", side_effect=run), contextlib.redirect_stdout(io.StringIO()):
                    if kind == "valid":
                        RUNNER.run_phase("owned-url", "/declared/go", phase, Path(temporary))
                    else:
                        with self.assertRaises(RuntimeError):
                            RUNNER.run_phase("owned-url", "/declared/go", phase, Path(temporary))

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
