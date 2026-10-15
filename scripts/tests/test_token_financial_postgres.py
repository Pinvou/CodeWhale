import importlib.util
import contextlib
import io
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "token_financial_postgres", Path(__file__).resolve().parents[1] / "test-token-financial-postgres.py"
)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class FinancialPostgresRunnerTests(unittest.TestCase):
    def test_disposable_loopback_url(self):
        raw = "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable"
        self.assertEqual(RUNNER.local_database_url(raw), raw)

    def test_deployed_or_ambiguous_connections_rejected(self):
        for raw in (
            "postgres://postgres@db.example:5433/postgres?sslmode=disable",
            "postgres://postgres@localhost:5433/postgres?sslmode=disable",
            "postgres://postgres@127.0.0.1:5433/customer?sslmode=disable",
            "postgres://postgres:secret@127.0.0.1:5433/postgres?sslmode=disable",
            "postgres://postgres@127.0.0.1/postgres?sslmode=disable",
            "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable&host=db.example",
            "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable#other",
        ):
            with self.subTest(raw=raw), self.assertRaises(ValueError):
                RUNNER.local_database_url(raw)

    def events(self, required=None, package=None, children=False):
        result = []
        for name in RUNNER.REQUIRED if required is None else required:
            result += [{"Action": "run", "Package": package, "Test": name}]
            if children:
                result += [{"Action": "run", "Package": package, "Test": name + "/actual"},
                           {"Action": "pass", "Package": package, "Test": name + "/actual"}]
            result += [{"Action": "pass", "Package": package, "Test": name}]
        if package is not None:
            result.append({"Action": "pass", "Package": package})
        return result

    def test_actual_required_suites(self):
        self.assertEqual(RUNNER.verify_results(self.events()), len(RUNNER.REQUIRED))

    def test_context_and_reconciliation_storage_regressions_are_required(self):
        required = {
            "TestContextEligibilityUsesCurrentScopedProductionRoutes",
            "TestReconciliationEvidenceMatchesPublicSchema",
            "TestF06OrganizationAdmissionBoundariesPublicPostgres",
        }
        self.assertTrue(required <= RUNNER.REQUIRED)
        for name in required:
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                RUNNER.verify_results([event for event in self.events() if event["Test"] != name])

    def test_zero_cases_is_not_success(self):
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results([{"Action": "pass", "Package": "token"}])

    def test_missing_suite_is_not_success(self):
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results(self.events()[1:])

    def test_skipped_required_subcase_fails_even_when_parent_passes(self):
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results(self.events() + [{"Action": "skip", "Test": sorted(RUNNER.REQUIRED)[0] + "/funds"}])

    def test_package_failure_is_not_success(self):
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results(self.events() + [{"Action": "fail", "Package": "token"}])

    def test_optional_external_skip_is_disclosed_and_allowed(self):
        self.assertEqual(
            RUNNER.verify_results(self.events() + [{"Action": "skip", "Test": "TestLiveExternalIssuer"}]),
            len(RUNNER.REQUIRED),
        )

    def run_main_fixture(self, *, existing_roles=False, user_objects=False, leftover=False,
                         owner_leftover=False, account_leftover=False, active_client=False, external_dependency=False,
                         go_fail_phase=None, empty_phase=None, skipped_phase=None):
        writes, commands, phases, trace = [], [], [], []
        state = {"bootstrapped": False, "account_done": False, "owner_done": False, "retained_done": False}

        def run(argv, **kwargs):
            if argv[0] == "go":
                commands.append(argv)
                if argv[-1] == "./internal/account":
                    phase, required, package = "owner-account", RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["account_done"] = True
                elif "^TestF06OrganizationWalletRequiresLifecycleEvidence$" in argv:
                    phase, required, package = "owner-wallet", RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["owner_done"] = True
                elif argv[-1] == "./internal/token":
                    phase, required, package = "retained-token", RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE
                    self.assertTrue(state["bootstrapped"])
                    state["retained_done"] = True
                else:
                    phase, required, package = "retained-billing", RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE
                    self.assertTrue(state["bootstrapped"])
                    state["retained_done"] = True
                phases.append(phase)
                trace.append(phase)
                self.assertNotIn("PINVOU_TEST_SCHEMA_DATABASE_URL", kwargs["env"])
                self.assertIn("-race", argv)
                if phase == "owner-account":
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_ACCOUNT_DATABASE_URL"], "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable")
                    self.assertNotIn("PINVOU_TEST_BILLING_DATABASE_URL", kwargs["env"])
                else:
                    self.assertNotIn("PINVOU_TEST_ACCOUNT_DATABASE_URL", kwargs["env"])
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_BILLING_DATABASE_URL"], "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable")
                if phase == "retained-token":
                    self.assertIn("PINVOU_TEST_TOKEN_DATABASE_URL", kwargs["env"])
                else:
                    self.assertNotIn("PINVOU_TEST_TOKEN_DATABASE_URL", kwargs["env"])
                events = self.events(required, package, children=phase != "retained-token")
                if empty_phase == phase:
                    events = [{"Action": "pass", "Package": package}]
                if skipped_phase == phase:
                    events.append({"Action": "skip", "Package": package, "Test": sorted(required)[0] + "/actual"})
                return SimpleNamespace(returncode=7 if phase == go_fail_phase else 0,
                                       stdout="\n".join(json.dumps(x) for x in events), stderr="")
            sql = argv[-1]
            if "CREATE ROLE" in sql:
                writes.append("create")
                trace.append("create")
                state["bootstrapped"] = True
                value = ""
            elif "DROP ROLE" in sql:
                writes.append("drop")
                trace.append("drop")
                state["bootstrapped"] = False
                value = ""
            elif "FROM pg_database" in sql:
                value = "postgres\ntemplate0\ntemplate1"
                if leftover and state["retained_done"]:
                    value += "\nunknown_fixture"
            elif "SELECT current_user" in sql:
                value = "postgres"
            elif "FROM pg_roles" in sql and "pg_shdepend" not in sql:
                value = "postgres"
                if existing_roles:
                    value += "\nexisting_runtime"
                elif account_leftover and state["account_done"]:
                    value += "\naccount_service"
                elif state["bootstrapped"] or owner_leftover and state["owner_done"]:
                    value = "\n".join(sorted(RUNNER.ACTORS | {"postgres"}))
            elif "FROM pg_class" in sql:
                value = "1" if user_objects else "0"
            elif "FROM pg_stat_activity" in sql:
                value = "1" if active_client else "0"
            elif "SHOW server_version_num" in sql:
                value = "170000"
            elif "pg_shdepend" in sql:
                value = "t" if external_dependency else "f"
            else:
                raise AssertionError("unexpected SQL")
            return SimpleNamespace(returncode=0, stdout=value, stderr="")
        with patch.dict(RUNNER.os.environ, {
            "PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL": "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable",
            "PINVOU_TEST_SCHEMA_DATABASE_URL": "must-not-reach-tests",
        }, clear=True), patch.object(RUNNER.subprocess, "run", side_effect=run), contextlib.redirect_stdout(io.StringIO()):
            try:
                RUNNER.main()
            finally:
                self.fixture_writes = writes
                self.fixture_commands = commands
                self.fixture_phases = phases
                self.fixture_trace = trace

    def test_existing_roles_prevent_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(existing_roles=True)
        self.assertEqual(self.fixture_writes, [])

    def test_user_objects_prevent_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(user_objects=True)
        self.assertEqual(self.fixture_writes, [])

    def test_unknown_remaining_database_prevents_next_phase_and_actor_cleanup(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(leftover=True)
        self.assertEqual(self.fixture_writes, ["create"])
        self.assertEqual(self.fixture_phases, ["owner-account", "owner-wallet", "retained-token"])

    def test_public_entry_executes_account_then_owner_token_and_retained(self):
        self.run_main_fixture()
        self.assertEqual(len(self.fixture_commands), 4)
        self.assertIn("^TestF06OrganizationWalletRequiresLifecycleEvidence$", self.fixture_commands[1])
        self.assertEqual(self.fixture_commands[0][-1], "./internal/account")
        self.assertEqual(self.fixture_commands[2][-1], "./internal/token")
        self.assertEqual(self.fixture_commands[3][-1], "./internal/billing")

    def test_owner_stage_restoration_is_required_before_retained_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(owner_leftover=True)
        self.assertEqual(self.fixture_writes, [])
        self.assertEqual(self.fixture_phases, ["owner-account", "owner-wallet"])

    def test_account_roles_must_be_restored_before_wallet_fixture(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(account_leftover=True)
        self.assertEqual(self.fixture_writes, [])
        self.assertEqual(self.fixture_phases, ["owner-account"])

    def test_account_phase_requires_both_real_cas_and_lifecycle_roots(self):
        self.assertEqual(RUNNER.ACCOUNT_REQUIRED, {
            "TestF06OrganizationStatusProjectionVersionPublicPostgres",
            "TestF06OrganizationDeletionVersionAndTerminalScope",
        })
        for name in RUNNER.ACCOUNT_REQUIRED:
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                RUNNER.verify_results([e for e in self.events(RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE, True) if e.get("Test", "").split("/")[0] != name], RUNNER.ACCOUNT_REQUIRED, package=RUNNER.ACCOUNT_PACKAGE, require_children=True, exact_roots=True)

    def test_active_client_prevents_any_fixture_execution(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(active_client=True)
        self.assertEqual(self.fixture_writes, [])
        self.assertEqual(self.fixture_commands, [])

    def test_external_role_dependencies_prevent_cleanup(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(external_dependency=True)
        self.assertEqual(self.fixture_writes, ["create"])

    def test_any_failed_phase_stops_later_phases(self):
        for phase, expected in [("owner-account", ["owner-account"]),
                                ("owner-wallet", ["owner-account", "owner-wallet"]),
                                ("retained-token", ["owner-account", "owner-wallet", "retained-token"]),
                                ("retained-billing", ["owner-account", "owner-wallet", "retained-token", "retained-billing"])]:
            with self.subTest(phase=phase), self.assertRaises(RuntimeError):
                self.run_main_fixture(go_fail_phase=phase)
            self.assertEqual(self.fixture_phases, expected)
            self.assertEqual(self.fixture_writes, [] if phase in {"owner-account", "owner-wallet"} else ["create", "drop"])

    def test_each_phase_rejects_zero_or_skipped_required_tests(self):
        for phase in ["owner-account", "owner-wallet", "retained-token", "retained-billing"]:
            for option in ["empty_phase", "skipped_phase"]:
                with self.subTest(phase=phase, option=option), self.assertRaises(RuntimeError):
                    self.run_main_fixture(**{option: phase})
                self.assertEqual(self.fixture_phases[-1], phase)

    def test_scoped_evidence_requires_exact_package_and_children_and_completion(self):
        valid = self.events(RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        self.assertEqual(RUNNER.verify_results(valid, RUNNER.OWNER_REQUIRED, package=RUNNER.BILLING_PACKAGE, require_children=True, exact_roots=True), 2)
        mutations = [
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in valid],
            [event for event in valid if event.get("Test")],
            [event for event in valid if "/" not in event.get("Test", "")],
            [event for event in valid if event["Action"] != "run"],
            valid + [{"Action": "pass", "Package": RUNNER.BILLING_PACKAGE, "Test": "TestUnrelated"}],
        ]
        for events in mutations:
            with self.subTest(events=events), self.assertRaises(RuntimeError):
                RUNNER.verify_results(events, RUNNER.OWNER_REQUIRED, package=RUNNER.BILLING_PACKAGE, require_children=True, exact_roots=True)

    def test_owned_fixture_restores_only_its_actors(self):
        self.run_main_fixture()
        self.assertEqual(self.fixture_writes, ["create", "drop"])
        self.assertEqual(self.fixture_trace, ["owner-account", "owner-wallet", "create", "retained-token", "retained-billing", "drop"])


if __name__ == "__main__":
    unittest.main()
