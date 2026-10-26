"""Fail-closed result validation for the real closure PostgreSQL runner."""
import importlib.util
import contextlib
import io
import json
import os
import re
from pathlib import Path
import unittest
import tempfile
from unittest import mock

SPEC = importlib.util.spec_from_file_location(
    "account_closure_postgres_runner", Path(__file__).resolve().parents[2] / "scripts/test-account-closure-postgres.py")
runner = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(runner)


def stream(*events):
    rows = [{"Package": "fixture", **event} for event in events]
    rows.append({"Action": "pass", "Package": "fixture"})
    return "\n".join(json.dumps(event) for event in rows)


class ClosurePostgresResults(unittest.TestCase):
    def test_real_migration_cli_roots_cannot_silently_drop(self):
        expected = {"TestAccountClosureLegacyRollbackCLI", "TestPhaseMigrationCLIPinsBeforeDown"}
        selected = [row for row in runner.CONTRACTS if row[0] == "migration-cli"]
        self.assertEqual(selected, [("migration-cli", "./cmd/migrate", expected, {})])
        for missing in expected:
            with self.subTest(missing=missing), self.assertRaises(RuntimeError):
                runner.verify_results(stream(*({"Action": "pass", "Test": name} for name in sorted(expected - {missing}))), expected)

    def test_current_membership_and_assessment_roots_cannot_silently_drop(self):
        original = {"TestClosedGrantPublishedOwnerEffects", "TestAccountClosureIsolationPreservesPriorAuditFacts",
                    "TestAccountClosureIsolationScrubsContactWithoutFinancialMutation",
                    "TestAccountClosurePreservesUnknownForcedRefundEvidence",
                    "TestAccountClosureRequiresAuthoritativeChargebackTerminal"}
        expected = original | {"TestAccountClosureAssessmentIncludesMemberReservations",
                               "TestF08MembershipClosureReasonQuotaReplayPostgres",
                               "TestF08ClosedGrantConcurrentReceiptReplayPostgres",
                               "TestF08ClosureBlocksPendingFinancialAnomalyPostgres",
                               "TestF08ReclaimBlocksPendingFinancialAnomalyPostgres"}
        self.assertEqual(runner.BILLING_ROOTS, expected)
        harness = (runner.ROOT / "account-platform/internal/billingschema/account_closed_grants_public_test.go").read_text(encoding="utf-8")
        selected = {name: (int(minimum), selection) for name, minimum, selection in re.findall(
            r'func (TestClosedGrantPublic\w+Harness)\(t \*testing.T\) \{\s*'
            r'(?:\s*//[^\n]*\n)*\s*runClosedGrantPublicOwnerHarness\(t, (\d+)\*time.Second, "\^\(([^"]+)\)\$"\)',
            harness)}
        self.assertEqual(set(selected), {parent for _, parent, _ in runner.CLOSED_GRANT_GROUPS})
        # Bind admission to the actual public callers, not just a separately
        # invoked budget helper. These are lower bounds from the failed CI,
        # with membership's final root having run only nine seconds there.
        observed_seconds = {"TestClosedGrantPublicOwnerHarness": 107.39,
                            "TestClosedGrantPublicFinancialHarness": 190.42,
                            "TestClosedGrantPublicMembershipHarness": 105.07}
        for _, parent, roots in runner.CLOSED_GRANT_GROUPS:
            minimum, selection = selected[parent]
            self.assertGreater(minimum, observed_seconds[parent])
            self.assertEqual(len(selection.split("|")), len(roots))
            self.assertEqual(set(selection.split("|")), roots)
        for missing in expected:
            with self.subTest(missing=missing), self.assertRaises(RuntimeError):
                runner.verify_results(stream(*({"Action": "pass", "Test": name} for name in sorted(expected - {missing}))), expected)
        events = [row for row in runner.CONTRACTS if row[0] == "membership-closure-event"]
        self.assertEqual(events, [("membership-closure-event", "./internal/billingschema",
                                 {"TestF08AccountClosureMembershipEventConsumerPublicPostgres"}, {})])

    def test_fixed_owner_groups_are_disjoint_complete_and_required_once(self):
        runner.validate_closed_grant_contracts(runner.CONTRACTS)
        selected = [row for row in runner.CONTRACTS if row[0].startswith("closed-grants")]
        self.assertEqual(len(selected), 3)
        self.assertEqual([len(row[3][""]) for row in selected], [4, 4, 2])
        roots = [root for row in selected for root in row[3][""]]
        self.assertEqual(len(roots), len(set(roots)))
        self.assertEqual(set(roots), runner.BILLING_ROOTS)
        for row in selected:
            for broken in [runner.CONTRACTS + [row], [item for item in runner.CONTRACTS if item is not row]]:
                with self.subTest(group=row[0]), self.assertRaises(RuntimeError):
                    runner.validate_closed_grant_contracts(broken)
        for missing in runner.BILLING_ROOTS:
            groups = [(name, parent, roots - {missing}) for name, parent, roots in runner.CLOSED_GRANT_GROUPS]
            with self.subTest(missing=missing), mock.patch.object(runner, "CLOSED_GRANT_GROUPS", groups), \
                    self.assertRaises(RuntimeError):
                runner.validate_closed_grant_contracts(runner.CONTRACTS)

    def test_duplicate_or_unexpected_root_is_not_success(self):
        for names in [["Owner", "Owner"], ["Owner", "Unexpected"]]:
            with self.subTest(names=names), self.assertRaises(RuntimeError):
                runner.verify_results(stream(*({"Action": "pass", "Test": name} for name in names)), {"Owner"})

    def run_public_wrapper(self, bad_group=None, failure=None, missing_root=None, platform="linux"):
        """Exercise the actual entry point with controlled process/PG replies."""
        calls = []
        last_group = None
        contracts = {"^(" + "|".join(sorted(roots)) + ")$": (name, roots, children)
                     for name, _, roots, children in runner.CONTRACTS}

        def process(argv, **kwargs):
            nonlocal last_group
            if argv[0] != "fixture-go":
                sql = argv[-1]
                calls.append(("query", sql))
                if sql == "SELECT current_user":
                    output = "fixture_owner"
                elif sql == "SELECT datname FROM pg_database ORDER BY datname":
                    output = "postgres\ntemplate0\ntemplate1"
                    if failure == "cleanup" and last_group == bad_group:
                        output += "\nretained_for_diagnosis"
                elif "SELECT rolname FROM pg_roles" in sql:
                    output = "fixture_owner"
                elif sql == "SHOW server_version_num":
                    output = "170011"
                else:
                    output = "0"
                return subprocess_result(output)
            name, roots, children = contracts[argv[-1]]
            last_group = name
            calls.append(("go", name, argv))
            for suffix, required in children.items():
                if name == bad_group and failure == "missing-child-process":
                    continue
                rows = [{"Action": "pass", "Test": root} for root in sorted(required)]
                if name == bad_group and missing_root:
                    rows = [row for row in rows if row["Test"] != missing_root]
                if name == bad_group and failure in {"fail", "skip"}:
                    rows[0]["Action"] = failure
                log = Path(kwargs["env"]["F08_TEST_CHILD_LOG"] + suffix)
                log.write_text(stream(*rows), encoding="utf-8")
            if name == bad_group and failure == "missing-parent-process":
                return subprocess_result("")
            return subprocess_result(stream(*({"Action": "pass", "Test": root} for root in sorted(roots))),
                                     1 if name == bad_group and failure == "process-failed" else 0)

        def subprocess_result(output, code=0):
            return mock.Mock(stdout=output, stderr="", returncode=code)

        error = None
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.dict(os.environ, {
                    "PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL": "postgres://fixture_owner@127.0.0.1:5433/postgres?sslmode=disable",
                    "PINVOU_ACCOUNT_CLOSURE_TEST_ARTIFACT_DIR": directory,
                }), mock.patch.object(runner.shutil, "which", return_value="fixture-go"), \
                mock.patch.object(runner.subprocess, "run", side_effect=process), \
                mock.patch.object(runner.sys, "platform", platform), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            try:
                runner.main()
            except RuntimeError as caught:
                error = caught
        return error, calls

    def test_public_entry_runs_each_fixed_group_with_cleanup_between_processes(self):
        for platform in ["linux", "win32"]:
            with self.subTest(platform=platform):
                error, calls = self.run_public_wrapper(platform=platform)
                self.assertIsNone(error)
                actual = [call for call in calls if call[0] == "go"]
                self.assertEqual([call[1] for call in actual], [row[0] for row in runner.CONTRACTS])
                for call in actual:
                    self.assertIn("-timeout=8m", call[2])
                    self.assertEqual("-race" in call[2], platform != "win32")
                positions = [i for i, call in enumerate(calls) if call[0] == "go"]
                for current, following in zip(positions, positions[1:]):
                    # The finally cleanup check and the next invocation's
                    # absence guard both precede the next process launch.
                    self.assertEqual(sum(call == ("query", "SELECT datname FROM pg_database ORDER BY datname")
                                         for call in calls[current + 1:following]), 2)

    def test_public_entry_rejects_each_missing_owner_child_root(self):
        for group, _, roots in runner.CLOSED_GRANT_GROUPS:
            for root in roots:
                with self.subTest(group=group, root=root):
                    error, calls = self.run_public_wrapper(bad_group=group, missing_root=root)
                    self.assertIsNotNone(error)
                    self.assertEqual([call[1] for call in calls if call[0] == "go"][-1], group)

    def test_public_entry_rejects_missing_failed_skipped_processes_and_retains_unknown_cleanup(self):
        for group, _, _ in runner.CLOSED_GRANT_GROUPS:
            for failure in ["missing-child-process", "missing-parent-process", "process-failed", "fail", "skip", "cleanup"]:
                with self.subTest(group=group, failure=failure):
                    error, calls = self.run_public_wrapper(bad_group=group, failure=failure)
                    self.assertIsNotNone(error)
                    self.assertEqual([call[1] for call in calls if call[0] == "go"][-1], group)

    def test_anonymization_advance_failure_root_cannot_silently_drop(self):
        expected = {"TestAccountClosureAnonymizesOwnedHistoricalContacts",
                    "TestAccountClosureRollbackPreservesEvidence",
                    "TestAccountClosureSelfServiceRecoveryPostgres",
                    "TestF08ClosureAdvanceFailureIsScheduledPostgres"}
        contracts = [row for row in runner.CONTRACTS if row[0] == "account-anonymization"]
        self.assertEqual(contracts, [("account-anonymization", "./internal/account", expected, {})])
        for missing in expected:
            with self.subTest(missing=missing), self.assertRaises(RuntimeError):
                runner.verify_results(stream(*({"Action": "pass", "Test": name}
                                              for name in sorted(expected - {missing}))), expected)

    def test_failed_child_diagnostics_are_visible_without_becoming_success(self):
        raw = stream({"Action": "output", "Test": "Root", "Output": "controlled failure detail\n"},
                     {"Action": "fail", "Test": "Root"})
        with tempfile.TemporaryDirectory() as directory:
            child_log = Path(directory) / "child.jsonl"
            child_log.write_text(raw, encoding="utf-8")
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                logs = runner.emit_child_logs(child_log, {"": {"Root"}, ".billing": {"Billing"}})
            self.assertIn("controlled failure detail", output.getvalue())
            self.assertNotIn(".billing", logs)
            with self.assertRaises(RuntimeError):
                runner.verify_results(logs[""], {"Root"})

    def test_counts_actual_parent_and_subcase_results(self):
        raw = stream({"Action": "pass", "Test": "Root/child"}, {"Action": "pass", "Test": "Root"})
        self.assertEqual(runner.verify_results(raw, {"Root"}), 2)

    def test_zero_tests_is_not_success(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Package": "fixture"}), {"Root"})

    def test_subcase_does_not_replace_required_root(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Root/child"}), {"Root"})

    def test_missing_required_child_is_not_success(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Owner"}), {"Owner", "Audit"})

    def test_skipped_subcase_fails_even_when_parent_passes(self):
        raw = stream({"Action": "skip", "Test": "Root/guard"}, {"Action": "pass", "Test": "Root"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_failed_package_is_not_hidden_by_passed_root(self):
        raw = stream({"Action": "pass", "Test": "Root"}, {"Action": "fail", "Package": "fixture"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_truncated_json_fails(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results('{"Action":', {"Root"})

    def test_empty_required_set_is_not_a_gate(self):
        with self.assertRaises(RuntimeError):
            runner.verify_results(stream({"Action": "pass", "Test": "Root"}), set())

    def test_valid_json_prefix_without_package_completion_fails(self):
        raw = json.dumps({"Action": "pass", "Test": "Root", "Package": "fixture"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_unfinished_sibling_fails_even_if_root_and_package_pass(self):
        raw = stream({"Action": "pass", "Test": "Root"}, {"Action": "run", "Test": "Root/sibling"})
        with self.assertRaises(RuntimeError):
            runner.verify_results(raw, {"Root"})

    def test_deployed_or_password_url_never_reaches_subprocess(self):
        for raw in ["postgres://user@db.example/postgres?sslmode=require",
                    "postgres://user:password@127.0.0.1:5433/postgres?sslmode=disable"]:
            with self.subTest(raw=raw), mock.patch.dict(os.environ, {"PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL": raw}), \
                    mock.patch.object(runner.subprocess, "run") as process:
                with self.assertRaises(ValueError):
                    runner.main()
                process.assert_not_called()


if __name__ == "__main__":
    unittest.main()
