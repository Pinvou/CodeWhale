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

    def events(self):
        return [{"Action": "pass", "Test": name} for name in RUNNER.REQUIRED]

    def test_actual_required_suites(self):
        self.assertEqual(RUNNER.verify_results(self.events()), len(RUNNER.REQUIRED))

    def test_context_and_reconciliation_storage_regressions_are_required(self):
        required = {
            "TestContextEligibilityUsesCurrentScopedProductionRoutes",
            "TestReconciliationEvidenceMatchesPublicSchema",
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
                         missing_target=None, skipped_target=None, failed_target=None, wrong_package=False):
        writes = []
        targets = []
        state = {"bootstrapped": False, "go_done": False}
        def run(argv, **kwargs):
            if argv[0] == "go":
                state["go_done"] = True
                target = argv[-1].removeprefix("./internal/")
                targets.append(target)
                self.assertNotIn("PINVOU_TEST_SCHEMA_DATABASE_URL", kwargs["env"])
                self.assertIn("-race", argv)
                required = RUNNER.REQUIRED if target == "token" else RUNNER.BILLING_REQUIRED
                events = [{"Action": "pass", "Test": name,
                           "Package": "github.com/Pinvou/pinvou3/account-platform/internal/" + ("other" if wrong_package else target)}
                          for name in sorted(required)]
                if missing_target == target:
                    events = events[1:]
                if skipped_target == target:
                    events.append({"Action": "skip", "Test": sorted(required)[0] + "/funds",
                                   "Package": "github.com/Pinvou/pinvou3/account-platform/internal/" + target})
                if target == "billing":
                    self.assertIn("-run", argv)
                    for name in required:
                        self.assertIn(name, argv[argv.index("-run") + 1])
                else:
                    self.assertNotIn("-run", argv)
                return SimpleNamespace(returncode=1 if failed_target == target else 0,
                                       stdout="\n".join(json.dumps(x) for x in events), stderr="")
            sql = argv[-1]
            if "CREATE ROLE" in sql:
                writes.append("create")
                state["bootstrapped"] = True
                value = ""
            elif "DROP ROLE" in sql:
                writes.append("drop")
                state["bootstrapped"] = False
                value = ""
            elif "FROM pg_database" in sql:
                value = "postgres\ntemplate0\ntemplate1"
                if leftover and state["go_done"]:
                    value += "\nunknown_fixture"
            elif "SELECT current_user" in sql:
                value = "postgres"
            elif "FROM pg_roles" in sql:
                value = "postgres"
                if existing_roles:
                    value += "\nexisting_runtime"
                elif state["bootstrapped"]:
                    value = "\n".join(sorted(RUNNER.ACTORS | {"postgres"}))
            elif "FROM pg_class" in sql:
                value = "1" if user_objects else "0"
            elif "SHOW server_version_num" in sql:
                value = "170000"
            elif "FROM pg_stat_activity" in sql:
                value = "0"
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
                self.fixture_targets = targets

    def test_existing_roles_prevent_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(existing_roles=True)
        self.assertEqual(self.fixture_writes, [])

    def test_user_objects_prevent_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(user_objects=True)
        self.assertEqual(self.fixture_writes, [])

    def test_unknown_remaining_database_prevents_actor_cleanup(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(leftover=True)
        self.assertEqual(self.fixture_writes, ["create"])

    def test_owned_fixture_restores_only_its_actors(self):
        self.run_main_fixture()
        self.assertEqual(self.fixture_writes, ["create", "drop"])
        self.assertEqual(self.fixture_targets, ["token", "billing"])

    def test_positive_extension_suites_are_mandatory(self):
        self.assertIn("TestF05AdditionalReservationProjectionPublicPostgres", RUNNER.REQUIRED)
        self.assertIn("TestHoldExtensionIdempotencePostgres", RUNNER.BILLING_REQUIRED)
        for target in ("token", "billing"):
            with self.subTest(target=target), self.assertRaises(RuntimeError):
                self.run_main_fixture(missing_target=target)

    def test_billing_skipped_child_cannot_be_hidden_by_parent_pass(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(skipped_target="billing")

    def test_unrelated_package_cannot_satisfy_required_suite_names(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(wrong_package=True)
        self.assertEqual(self.fixture_targets, ["token"])

    def test_failed_token_phase_stops_before_billing(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(failed_target="token")
        self.assertEqual(self.fixture_targets, ["token"])

    def test_failed_billing_phase_fails_the_runner(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(failed_target="billing")
        self.assertEqual(self.fixture_targets, ["token", "billing"])


if __name__ == "__main__":
    unittest.main()
