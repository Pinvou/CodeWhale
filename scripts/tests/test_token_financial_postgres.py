import importlib.util
import contextlib
import io
import json
import re
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location(
    "token_financial_postgres", Path(__file__).resolve().parents[1] / "test-token-financial-postgres.py"
)
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


CHECKPOINT_LEAF_ROOTS = {
    "TestRegistrationBonusEligibilityCheckpointPostgresIntegration",
    "TestRegistrationBonusCheckpointGuardsPostgresIntegration",
    "TestRegistrationBonusCheckpointMigrationPostgresIntegration",
    "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration",
    "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration",
}
CHECKPOINT_FOREIGN_ROOT = "TestRegistrationBonusCheckpointForeignFactsPostgresIntegration"
CHECKPOINT_FOREIGN_CHILDREN = {"unbound-existing-facts", "foreign-key-before-finalize"}


class FinancialPostgresRunnerTests(unittest.TestCase):
    def test_catalog_rollback_requires_all_historical_versions_and_actual_cli(self):
        root = "TestBillingHistoricalCatalogRollbackPostgres"
        self.assertIn(root, RUNNER.SCHEMA_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-schema")]
        self.assertIn(root, command[command.index("-run") + 1])
        expected = {
            f"version{version}/seeded={seeded}{suffix}"
            for version in (35, 36, 37, 51)
            for seeded in ("false", "true")
            for suffix in ("", "/actual-cli")
        }
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], expected)
        events = self.events({root}, RUNNER.SCHEMA_PACKAGE, children=True)
        options = {"package": RUNNER.SCHEMA_PACKAGE, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in expected:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.SCHEMA_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_password_preparation_requires_redaction_boundaries_and_reference_safety(self):
        root = "TestPasswordPreparationAuthorizationPostgres"
        self.assertIn(root, RUNNER.ACCOUNT_REQUIRED)
        required = {
            "retention-redacts-expired-mobiles-without-changing-grant-facts/expired",
            "retention-redacts-expired-mobiles-without-changing-grant-facts/young",
            "retention-redacts-expired-mobiles-without-changing-grant-facts/valid",
            "retention-keeps-issued-grant-and-continues-later-artifact-cleanup",
            "retention-skips-locked-expired-captcha-and-rejects-late-grant",
            "retention-FK-protects-inflight-reference-and-subsequent-passes",
        }
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], required)
        events = self.events({root}, RUNNER.ACCOUNT_PACKAGE, children=True)
        options = {"package": RUNNER.ACCOUNT_PACKAGE, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in required:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_token_public_upgrade_phase_requires_retained_pre_cleanup_stage(self):
        root = "TestManagementPublicMigrationInstallation"
        self.assertEqual(RUNNER.TOKEN_SCHEMA_REQUIRED, {root})
        self.run_main_fixture()
        index = self.fixture_phases.index("owner-token-schema")
        command = self.fixture_commands[index]
        self.assertEqual(command[-1], "./internal/tokenschema")
        self.assertEqual(command[command.index("-run") + 1], "^" + root + "$")
        self.assertLess(index, self.fixture_phases.index("owner-provisioning"))
        events = self.events({root}, RUNNER.TOKEN_SCHEMA_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_SCHEMA_PACKAGE, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in RUNNER.EXACT_REQUIRED_CHILDREN[root]:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_SCHEMA_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)
        for option in ("empty_phase", "skipped_phase", "go_fail_phase"):
            with self.subTest(option=option), self.assertRaises(RuntimeError):
                self.run_main_fixture(**{option: "owner-token-schema"})
            self.assertEqual(self.fixture_phases[-1], "owner-token-schema")
            self.assertEqual(self.fixture_writes, [])

    def test_pending_expiry_requires_owner_windows_and_historical_fallback(self):
        required = {
            "TestUserUnusedIntentCancellationOwnerPostgres": {
                "before-claim", "after-claim", "waiting-owner-does-not-starve-later-target", "acquired-intent-control", "model-first-control",
            },
            "TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres": {
                "infinity", "tomorrow", "zero-time", "invalid-calendar",
                "bad-first-valid-later", "wrong-identity-first-valid-later", "valid-first-later-bad",
            },
            "TestPendingUnusedExpiryCancellationOwnerPostgres": {
                "model-claim-race",
                "user-first-and-retry",
                "model-after-user-command",
                "model-review-after-lost-release-ack",
                "prior-user-stop",
                "no-intent-control",
                "waiting-owner-does-not-starve-later-target",
            },
            "TestPendingUnusedExpiryLegacyCancellationFallbackPostgres": {
                "model-origin", "user-local-pre-dispatch",
            },
        }
        for root, children in required.items():
            with self.subTest(root=root):
                self.assertIn(root, RUNNER.REQUIRED)
                self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
                self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
                events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
                options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
                RUNNER.verify_results(events, {root}, **options)
                for invalid in (
                    [event for event in events if event.get("Test", "").split("/", 1)[0] != root],
                    events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root}],
                ):
                    with self.assertRaises(RuntimeError):
                        RUNNER.verify_results(invalid, {root}, **options)
                for child in children:
                    for invalid in (
                        [event for event in events if event.get("Test") != root + "/" + child],
                        events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
                    ):
                        with self.subTest(child=child), self.assertRaises(RuntimeError):
                            RUNNER.verify_results(invalid, {root}, **options)

    def test_model_terminal_observation_requires_all_windows_and_negative_probes(self):
        required = {
            "TestEmergencyCancellationClaimOwnershipPostgres": {"before-request", "after-request", "pending", "zero-attempt"},
            "TestEmergencyCancellationRequestLocksClaimPostgres": {"usage-lock-reclaim"},
            "TestModelTerminalObservationPreservesExpiryProofPostgres": {
                "hold-attempted",
                "hold-converged",
                "hold-requested",
                "intent-attempted",
                "intent-converged",
                "intent-requested",
                "user-intent-attempted",
                "user-intent-converged",
                "user-intent-requested",
            },
            "TestModelTerminalObservationRejectsUntrustedEvidencePostgres": {
                "attempt-aborts",
                "attempt-provider-success",
                "attempt-releases",
                "converged-extra-field",
                "convergence-wrong-source",
                "extra-field",
                "missing-field",
                "not-terminal",
                "orphan-request",
                "pre-dispatch",
                "provider-reference",
                "provider-success",
                "unknown-capability",
                "upstream-reference",
                "wrong-adapter",
            },
        }
        for root, children in required.items():
            with self.subTest(root=root):
                self.assertIn(root, RUNNER.REQUIRED)
                self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
                self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
                events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
                options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
                RUNNER.verify_results(events, {root}, **options)
                for child in children:
                    for invalid in (
                        [event for event in events if event.get("Test") != root + "/" + child],
                        events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
                    ):
                        with self.subTest(child=child), self.assertRaises(RuntimeError):
                            RUNNER.verify_results(invalid, {root}, **options)

    def test_terminal_receipt_requires_model_and_expiry_replay_interleavings(self):
        root = "TestCancellationFirstUserPreservesConfirmedReceiptPostgres"
        children = {"model-before-attempt", "expiry-proof-ack-replay"}
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_cancellation_confirmation_requires_both_reservation_ack_interleavings(self):
        root = "TestCancellationReceivedReadRechecksReservationPostgres"
        children = {"not-started", "reserve-ack-pending", "reserve-ack-attached", "wrong-hold-then-original"}
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_forced_completion_roots_and_failure_children_are_required(self):
        children = {
            "TestEscalatedForcedRefundAtomicRollbackPostgres": {
                "terminal-statement", "receipt-statement", "deferred-commit",
            },
            "TestEscalatedForcedRefundEvidenceAndReceiptGuardsPostgres": {
                "manual-status", "manual-link", "order-link", "transaction-link",
                "evidence-link", "receipt-identity-and-terminal-replay",
            },
        }
        roots = set(children) | {
            "TestEscalatedForcedRefundRequiresSecondActorAndReplays",
            "TestOrdinaryForcedRefundAppliedReceiptRecoveryPostgres",
        }
        self.assertTrue(roots <= RUNNER.RETAINED_REQUIRED)
        for root in roots:
            self.assertEqual(root in RUNNER.BILLING_CHILD_REQUIRED, root in children)
        for root, required_children in children.items():
            self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], required_children)
            events = self.events({root}, RUNNER.BILLING_PACKAGE, children=True)
            options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True}
            RUNNER.verify_results(events, {root}, **options)
            for child in required_children:
                for invalid in (
                    [event for event in events if event.get("Test") != root + "/" + child],
                    events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/" + child}],
                    events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/" + child}],
                ):
                    with self.subTest(root=root, child=child), self.assertRaises(RuntimeError):
                        RUNNER.verify_results(invalid, {root}, **options)

    def setUp(self):
        # Mock local-fixture tests must not inherit the live consumer's CI binding.
        environment = patch.dict(RUNNER.os.environ, {"GITHUB_ACTIONS": "",
            "PINVOU_ACCOUNT_CI_PG_PORT": "", "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID": "",
            "PINVOU_ACCOUNT_CI_PG_DIRECTORY": ""})
        environment.start()
        self.addCleanup(environment.stop)

    def test_orphan_order_requires_fresh_and_upgrade_persistence(self):
        root = "TestReconciliationOrphanPaidOrderPostgres"
        children = {"fresh-install", "historical-upgrade"}
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIsNotNone(re.fullmatch(command[command.index("-run") + 1], root))
        events = self.events({root}, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_unknown_date_orphan_requires_cross_date_lifecycle(self):
        root = "TestReconciliationUnknownDateOrphanLifecyclePostgres"
        children = {"cross-date-concurrency", "legacy-and-ignore", "observation-rollback", "stale-review-binding"}
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIsNotNone(re.fullmatch(command[command.index("-run") + 1], root))
        events = self.events({root}, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_remote_cancellation_requires_owner_loop_transport_cases(self):
        root = "TestRemoteModelCancellationOwnerLoopTransport"
        children = {"remote_emergency", "remote_emergency_then_user", "user_only",
                    "read_failure", "lease_failure", "natural_terminal",
                    "initial_requested", "initial_read_failure", "initial_lease_failure"}
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-token")]
        self.assertNotIn("-run", command)
        self.assertIsNone(re.fullmatch(command[command.index("-skip") + 1], root))
        events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_local_cancellation_handshake_requires_all_http_interleavings(self):
        root = "TestLocalModelCancellationHandshakeHTTP"
        children = {"before_registration", "during_persistent_read", "after_persistent_read",
                    "read_failure_preserves_funds", "normal", "user_stop_preserves_generation",
                    "emergency_then_user_before_registration"}
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], children)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-token")]
        self.assertNotIn("-run", command)
        self.assertIsNone(re.fullmatch(command[command.index("-skip") + 1], root))
        events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in children:
            for invalid in (
                [event for event in events if event.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_lifecycle_reconciliation_has_an_exclusive_exact_account_phase(self):
        root = "TestLifecycleV3ApprovalReconciliationPostgres"
        required = {"wrong_durable_fail_closed", "safe_v3_false_true_and_v2_dry_apply_next_tick",
                    "expired_approved_v3_missing_history", "expired_execution_failed_v3_missing_history",
                    "unknown_version_fail_closed"}
        self.assertEqual(RUNNER.LIFECYCLE_RECONCILIATION_REQUIRED, {root})
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[root], required)
        self.assertNotIn(root, RUNNER.ACCOUNT_REQUIRED)
        self.run_main_fixture()
        index = self.fixture_phases.index("lifecycle-reconciliation")
        command = self.fixture_commands[index]
        self.assertEqual(command[-1], "./internal/account")
        self.assertEqual(command[command.index("-run") + 1], "^" + root + "$")
        self.assertLess(self.fixture_trace.index("lifecycle-reconciliation"), self.fixture_trace.index("create"))
        events = self.events({root}, RUNNER.ACCOUNT_PACKAGE, children=True)
        options = {"package": RUNNER.ACCOUNT_PACKAGE, "exact_roots": True, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in required:
            for invalid in (
                [e for e in events if e.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)
        for option in ("empty_phase", "skipped_phase", "go_fail_phase"):
            with self.subTest(option=option), self.assertRaises(RuntimeError):
                self.run_main_fixture(**{option: "lifecycle-reconciliation"})
            self.assertEqual(self.fixture_phases[-1], "lifecycle-reconciliation")
            self.assertEqual(self.fixture_writes, [])

    def test_lifecycle_approval_requires_both_protocols_and_force_variant(self):
        root = "TestLifecycleForceCancelApprovalPostgres"
        variants = {"v2-false-compatibility", "v3-false", "v3-true-removed",
                    "v3-true-removed/emergency-then-user-origin-retains-authority",
                    "v3-true-removed/emergency-then-user-origin-retains-authority/owner-heartbeat-observes-remote-request"}
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN.get(root), variants)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-token")]
        self.assertNotIn("-run", command)
        self.assertIsNone(re.fullmatch(command[command.index("-skip") + 1], root))
        events = self.events({root}, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True}
        RUNNER.verify_results(events, {root}, **options)
        for child in variants:
            for invalid in (
                [e for e in events if e.get("Test") != root + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": root + "/" + child}],
            ):
                with self.subTest(variant=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, {root}, **options)

    def test_registration_compensation_selects_every_database_and_business_case(self):
        root = "TestRegistrationCompensationDatabaseErrorIdentityPostgres"
        required = {
            "operation-read", "absence-proof-read", "completion-write",
            "missing-completion-remains-business-rejection",
            "noncompensable-registration-remains-rejected",
        }
        self.assertIn(root, RUNNER.ACCOUNT_REQUIRED)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN.get(root), required)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(root, command[command.index("-run") + 1])
        package = RUNNER.ACCOUNT_PACKAGE
        events = self.events({root}, package, children=True)
        options = {"package": package, "require_children": True, "exact_roots": True}
        RUNNER.verify_results(events, {root}, **options)
        invalid_events = [
            [],
            [e for e in events if not (e.get("Test") == root and e.get("Action") == "run")],
            [e for e in events if "/" not in e.get("Test", "")],
            events + [{"Action": "skip", "Package": package, "Test": root}],
            events + [{"Action": "skip", "Package": package, "Test": root + "/operation-read"}],
            events + [{"Action": "fail", "Package": package, "Test": root + "/operation-read"}],
        ]
        invalid_events.extend([e for e in events if e.get("Test") != root + "/" + child] for child in required)
        for invalid in invalid_events:
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {root}, **options)

    def test_restored_reads_require_real_public_migration_postgres_execution(self):
        name = "TestRestoredAdminReadsWithPublicMigrationsAndRuntimeActor"
        self.assertIn(name, RUNNER.ACCOUNT_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, command[command.index("-run") + 1])
        events = self.events(RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE, children=True)
        options = {"package": RUNNER.ACCOUNT_PACKAGE, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, RUNNER.ACCOUNT_REQUIRED, **options), 0)
        for action in ["skip", "fail"]:
            with self.subTest(action=action), self.assertRaises(RuntimeError):
                RUNNER.verify_results(events + [{"Action": action, "Package": RUNNER.ACCOUNT_PACKAGE, "Test": name}], RUNNER.ACCOUNT_REQUIRED, **options)

    def test_registration_bonus_checkpoint_roots_are_mandatory(self):
        names = CHECKPOINT_LEAF_ROOTS | {CHECKPOINT_FOREIGN_ROOT}
        self.assertTrue(names <= RUNNER.ACCOUNT_REQUIRED)
        self.assertEqual(RUNNER.ACCOUNT_LEAF_REQUIRED, CHECKPOINT_LEAF_ROOTS)
        self.assertEqual(RUNNER.ACCOUNT_CHILD_REQUIRED, RUNNER.ACCOUNT_REQUIRED - CHECKPOINT_LEAF_ROOTS)
        self.assertEqual(RUNNER.EXACT_REQUIRED_CHILDREN[CHECKPOINT_FOREIGN_ROOT], CHECKPOINT_FOREIGN_CHILDREN)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        pattern = command[command.index("-run") + 1]
        events = self.account_checkpoint_shape(self.events(RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE, children=True))
        options = {"package": RUNNER.ACCOUNT_PACKAGE, "exact_roots": True, "require_children": True,
                   "children_required": RUNNER.ACCOUNT_CHILD_REQUIRED}
        RUNNER.verify_results(events, RUNNER.ACCOUNT_REQUIRED, **options)
        for leaf in CHECKPOINT_LEAF_ROOTS:
            self.assertFalse(any(e.get("Test", "").startswith(leaf + "/") for e in events))
        for name in names:
            self.assertIn(name, pattern)
            for invalid in (
                [e for e in events if e.get("Test", "").split("/", 1)[0] != name],
                [e for e in events if not (e.get("Test") == name and e.get("Action") == "run")],
                [e for e in events if not (e.get("Test") == name and e.get("Action") == "pass")],
                events + [{"Action": "skip", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": name}],
                events + [{"Action": "fail", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": name}],
            ):
                with self.subTest(root=name), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, RUNNER.ACCOUNT_REQUIRED, **options)
        for child in CHECKPOINT_FOREIGN_CHILDREN:
            for invalid in (
                [e for e in events if e.get("Test") != CHECKPOINT_FOREIGN_ROOT + "/" + child],
                events + [{"Action": "skip", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": CHECKPOINT_FOREIGN_ROOT + "/" + child}],
                events + [{"Action": "fail", "Package": RUNNER.ACCOUNT_PACKAGE, "Test": CHECKPOINT_FOREIGN_ROOT + "/" + child}],
            ):
                with self.subTest(child=child), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, RUNNER.ACCOUNT_REQUIRED, **options)
        invalid = [e for e in events if not e.get("Test", "").startswith(CHECKPOINT_FOREIGN_ROOT + "/")]
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results(invalid, RUNNER.ACCOUNT_REQUIRED, **options)

    def account_checkpoint_shape(self, events):
        # The five real Go roots are leaves. The sixth has these two real children,
        # never the generic helper's invented /actual child.
        return [e for e in events
                if not (e.get("Test", "").split("/", 1)[0] in CHECKPOINT_LEAF_ROOTS and "/" in e.get("Test", ""))
                and e.get("Test") != CHECKPOINT_FOREIGN_ROOT + "/actual"]

    def test_duplicate_forced_refund_requires_all_four_order_and_proof_cases(self):
        root = "TestLedgerRepairAuthorityDuplicateForcedRefundPostgres"
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        RUNNER.verify_results(events, RUNNER.RETAINED_REQUIRED, **options)
        for child in RUNNER.EXACT_REQUIRED_CHILDREN[root]:
            reduced = [e for e in events if e.get("Test") != root + "/" + child]
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(reduced, RUNNER.RETAINED_REQUIRED, **options)

    def test_preparation_ack_pg_evidence_is_required(self):
        name = "TestPreparationFailurePreservesAcceptedUsagePostgres"
        self.assertIn(name, RUNNER.REQUIRED)
        self.assertIn(name, RUNNER.TOKEN_CHILD_REQUIRED)
        events = self.events(RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True,
                   "children_required": RUNNER.TOKEN_CHILD_REQUIRED}
        self.assertGreater(RUNNER.verify_results(events, **options), 0)
        for invalid in (
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": name}],
            events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": name + "/actual"}],
        ):
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, **options)

    def test_cumulative_forced_refund_audit_is_selected_and_requires_child(self):
        name = "TestF03CumulativeForcedRefundReceiptAudit"
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIsNotNone(re.fullmatch(command[command.index("-run") + 1], name))
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events({name}, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {name}}
        self.assertGreater(RUNNER.verify_results(events, {name}, **options), 0)
        for invalid in ([], [e for e in events if "/" not in e.get("Test", "")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)

    def test_reconciliation_ledger_repair_pg_leaf_is_selected_and_cannot_skip(self):
        for name in ("TestLedgerRepairProofHistoricalUpgradePostgres", "TestReconciliationLedgerRepairPublicPostgres", "TestReconciliationLedgerRepairForcedPeerPostgres", "TestLedgerRepairAuthorityMixedBatchPostgres", "TestLedgerRepairHistoryCapabilityPostgres"):
            with self.subTest(name=name):
                self._assert_reconciliation_repair_leaf(name)

    def _assert_reconciliation_repair_leaf(self, name):
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIsNotNone(re.fullmatch(command[command.index("-run") + 1], name))
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_LEAF_REQUIRED)
        events = self.events({name}, RUNNER.BILLING_PACKAGE)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": set()}
        self.assertEqual(RUNNER.verify_results(events, {name}, **options), 1)
        for invalid in ([], [e for e in events if e.get("Test") != name],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name}]):
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)

    def test_callback_http_admission_pg_leaf_is_selected_and_cannot_skip(self):
        name = "TestCallbackHTTPAdmissionPersistsPostgres"
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        selection = command[command.index("-run") + 1]
        self.assertIsNotNone(re.fullmatch(selection, name),
                             "callback PG root absent from retained-billing -run selection")
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_LEAF_REQUIRED)
        self.assertNotIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events({name}, RUNNER.BILLING_PACKAGE)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": set()}
        # This real Go test is a named leaf: run + pass + package completion is
        # necessary, while fabricated child-case evidence is not required.
        self.assertEqual(RUNNER.verify_results(events, {name}, **options), 1)
        for invalid in (
                [],
                [event for event in events if event.get("Action") != "run"],
                [event for event in events if event.get("Test") != name],
                [event for event in events if event.get("Test")],
                events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/unexpected"}],
                events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in events]):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)

    def test_circuit_failure_pg_evidence_is_required(self):
        name = "TestCircuitFailureBeforeDispatchPostgres"
        self.assertIn(name, RUNNER.REQUIRED)
        self.assertIn(name, RUNNER.TOKEN_CHILD_REQUIRED)
        events = self.events(RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "require_children": True,
                   "children_required": RUNNER.TOKEN_CHILD_REQUIRED}
        self.assertGreater(RUNNER.verify_results(events, **options), 0)
        invalid_sets = [
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": name}],
            events + [{"Action": "skip", "Package": RUNNER.TOKEN_PACKAGE, "Test": name + "/actual"}],
        ]
        for invalid in invalid_sets:
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, **options)

    def test_credential_failure_pg_roots_cannot_disappear_or_skip(self):
        roots = {
            "TestCredentialRotationWindowReceiptPostgres",
            "TestCredentialRotationOtherActorEffectPostgres",
            "TestCredentialFailurePersistenceBoundariesPostgres",
            "TestCredentialFailurePreparationFencePostgres",
            "TestCredentialFailureExistingEventsPostgres",
        }
        children = {"TestCredentialRotationWindowReceiptPostgres",
                    "TestCredentialFailurePersistenceBoundariesPostgres"}
        self.assertTrue(roots <= RUNNER.REQUIRED)
        self.assertTrue(children <= RUNNER.TOKEN_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-token")]
        # This phase already runs the whole package with its PostgreSQL URL.
        # Required sets enforce evidence; they are not a -run selection filter.
        self.assertNotIn("-run", command)
        self.assertIn("-skip", command)
        self.assertEqual(command[command.index("-skip") + 1],
                         "^(" + "|".join(sorted(RUNNER.PROVISIONING_REQUIRED)) + ")$")
        package = RUNNER.TOKEN_PACKAGE
        events = self.events(RUNNER.REQUIRED, package, children=True)
        options = {"package": package, "require_children": True,
                   "children_required": RUNNER.TOKEN_CHILD_REQUIRED}
        self.assertGreater(RUNNER.verify_results(events, **options), 0)
        for name in roots:
            for invalid in (
                [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
                [event for event in events if not (event.get("Test") == name and event.get("Action") == "run")],
                events + [{"Action": "skip", "Package": package, "Test": name}],
                events + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
                events + [{"Action": "fail", "Package": package, "Test": name + "/actual"}],
                [{**event, "Package": RUNNER.BILLING_PACKAGE}
                 if event.get("Test", "").split("/", 1)[0] == name else event for event in events],
            ):
                with self.subTest(name=name, events=len(invalid)), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, **options)
        for name in children:
            with self.subTest(no_children=name), self.assertRaises(RuntimeError):
                RUNNER.verify_results([event for event in events
                                       if not event.get("Test", "").startswith(name + "/")], **options)

    def test_retained_billing_budget_preserves_all_phase_commands_and_cleanup(self):
        self.run_main_fixture()
        self.assertEqual(self.fixture_phases, [
            "owner-account", "owner-wallet", "owner-schema", "owner-token-schema",
            "owner-provisioning", "lifecycle-reconciliation", "retained-token", "retained-billing",
        ])
        for phase, command in zip(self.fixture_phases, self.fixture_commands, strict=True):
            with self.subTest(phase=phase):
                budgets = [arg for arg in command if arg.startswith("-timeout=")]
                self.assertEqual(budgets, ["-timeout=75m" if phase == "retained-billing" else "-timeout=20m"])
                self.assertIn("-race", command)
                self.assertIn("-count=1", command)
        billing = self.fixture_commands[-1]
        self.assertEqual(billing[-1], "./internal/billing")
        self.assertEqual(billing[billing.index("-run") + 1],
                         "^(" + "|".join(sorted(RUNNER.RETAINED_REQUIRED)) + ")$")
        self.assertEqual(len(RUNNER.RETAINED_REQUIRED), 75)
        self.assertEqual(self.fixture_writes, ["create", "drop"])
        self.assertEqual(self.fixture_trace, [
            "owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning",
            "lifecycle-reconciliation", "create", "retained-token", "retained-billing", "drop",
        ])

    def test_account_job_budget_includes_retained_billing_and_later_closure(self):
        workflow = (Path(__file__).resolve().parents[2] / ".github/workflows/pr-check.yml").read_text(encoding="utf-8")
        job = workflow.split("\n  account-platform-test:\n", 1)[1].split("\n  windows-relay-dacl:\n", 1)[0]
        self.assertIn("\n    timeout-minutes: 180\n", job)
        self.assertIn("python3 scripts/test-financial-parallel.py", job)
        self.assertIn("python3 scripts/test-account-closure-postgres.py", job)

    def test_withdrawal_original_receipt_is_selected_and_requires_real_children(self):
        name = "TestApprovalWithdrawalOriginalReceiptPostgres"
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        self.assertIn(name, roots)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, command[command.index("-run") + 1])
        events = self.events(roots, package, children=True)
        options = {"package": package, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, roots, **options), 0)
        for invalid in (
            [],
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            [event for event in events if not (event.get("Test", "") == name and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": package, "Test": name}],
            events + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            events + [{"Action": "fail", "Package": package, "Test": name + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} if event.get("Test", "").split("/", 1)[0] == name else event for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


    def test_query_command_and_actual_client_roots_are_strongly_selected(self):
        roots = {"TestLatePaymentQueryCommandPostgres", "TestLatePaymentQueryClientHTTPPostgres"}
        self.assertTrue(roots <= RUNNER.RETAINED_REQUIRED)
        self.assertTrue(roots <= RUNNER.BILLING_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        for root in roots:
            self.assertIn(root, command[command.index("-run") + 1])
        package = RUNNER.BILLING_PACKAGE
        options = {"package": package, "exact_roots": True, "require_children": True}
        valid = self.events(roots, package, children=True)
        self.assertGreater(RUNNER.verify_results(valid, roots, **options), 0)
        for root in roots:
            for invalid in (
                [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
                [event for event in valid if not event.get("Test", "").startswith(root + "/")],
                valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
                valid + [{"Action": "fail", "Package": package, "Test": root + "/actual"}],
            ):
                with self.subTest(root=root, invalid=invalid), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, roots, **options)


    def test_query_forward_identity_is_in_schema_selection(self):
        root = "TestLatePaymentQueryRegisteredIdentity"
        self.assertIn(root, RUNNER.SCHEMA_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-schema")]
        self.assertIn(root, command[command.index("-run") + 1])


    def test_late_schedule_receipt_root_is_selected_and_requires_completed_children(self):
        root = "TestLatePaymentScheduleCommandReplayPostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(root, command[command.index("-run") + 1])
        events = self.events({root}, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {root}}
        RUNNER.verify_results(events, {root}, **options)
        for invalid in (
            [],
            [event for event in events if event.get("Test") != root],
            [event for event in events if "/" not in event.get("Test", "")],
            [event for event in events if not (event.get("Test") == root and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
            events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
            events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {root}, **options)

    def test_hold_acknowledgement_recovery_root_and_children_are_mandatory(self):
        root = "TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres"
        self.assertIn(root, RUNNER.REQUIRED)
        self.assertIn(root, RUNNER.TOKEN_CHILD_REQUIRED)
        events = self.events(required={root}, package=RUNNER.TOKEN_PACKAGE, children=True)
        options = {"package": RUNNER.TOKEN_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {root}}
        RUNNER.verify_results(events, {root}, **options)
        for rejected in ([event for event in events if event.get("Test") != root],
                         [event for event in events if "/" not in event.get("Test", "")],
                         [dict(event, Action="skip") if "/" in event.get("Test", "") and event.get("Action") == "pass" else event for event in events],
                         [dict(event, Action="fail") if "/" in event.get("Test", "") and event.get("Action") == "pass" else event for event in events]):
            with self.subTest(events=rejected), self.assertRaises(RuntimeError):
                RUNNER.verify_results(rejected, {root}, **options)


    def test_reconciliation_fair_retry_is_selected_and_requires_real_children(self):
        root = "TestReconciliationFairRetryRuntimePostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(root, command[command.index("-run") + 1])
        events = self.events(required={root}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {root}}
        RUNNER.verify_results(events, {root}, **options)
        for rejected in ([],
                         [event for event in events if event.get("Test") != root],
                         [event for event in events if "/" not in event.get("Test", "")],
                         [event for event in events if not (event.get("Test") == root and event.get("Action") == "run")],
                         events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
                         events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
                         [dict(event, Package=RUNNER.TOKEN_PACKAGE) for event in events]):
            with self.subTest(events=len(rejected)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(rejected, {root}, **options)


    def test_reconciliation_retry_identity_root_is_selected_and_cannot_be_faked(self):
        root = "TestReconciliationFairRetryRegisteredIdentity"
        self.assertEqual(len(RUNNER.SCHEMA_REQUIRED), 12)
        self.assertIn(root, RUNNER.SCHEMA_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-schema")]
        self.assertIn(root, command[command.index("-run") + 1])
        roots, package = RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE
        valid = self.events(roots, package, children=True)
        options = {"package": package, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(valid, roots, **options), 0)
        for invalid in (
            [],
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
            [event for event in valid if not event.get("Test", "").startswith(root + "/")],
            [event for event in valid if not (event.get("Test") == root and event["Action"] == "run")],
            valid + [{"Action": "skip", "Package": package, "Test": root}],
            valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
            valid + [{"Action": "fail", "Package": package, "Test": root}],
            valid + [{"Action": "fail", "Package": package, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.BILLING_PACKAGE} if event.get("Test", "").split("/", 1)[0] == root else event for event in valid],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


    def test_refund_network_boundary_requires_completed_children(self):
        root = "TestRefundProviderNetworkOutsideFinancialTransactionPostgres"
        self.assertIn(root, RUNNER.REFUND_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        valid = self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        RUNNER.verify_results(valid, RUNNER.RETAINED_REQUIRED, **options)
        for invalid in ([e for e in valid if e.get("Test", "").split("/", 1)[0] != root],
                        [e for e in valid if not e.get("Test", "").startswith(root + "/")],
                        valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}]):
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, RUNNER.RETAINED_REQUIRED, **options)


    def test_approval_list_pool_root_is_selected_and_requires_real_children(self):
        name = "TestApprovalListPoolRuntimePostgres"
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        self.assertIn(name, roots)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, command[command.index("-run") + 1])
        events = self.events(roots, package, children=True)
        options = {"package": package, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, roots, **options), 0)
        for invalid in (
            [],
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            [event for event in events if not (event.get("Test", "") == name and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": package, "Test": name}],
            events + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            events + [{"Action": "fail", "Package": package, "Test": name + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} if event.get("Test", "").split("/", 1)[0] == name else event for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


    def test_forced_refund_member_terminal_root_and_children_are_mandatory(self):
        name = "TestForcedRefundMemberGrantTerminalStatePostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        RUNNER.verify_results(events, {name}, package=RUNNER.BILLING_PACKAGE, require_children=True)
        for rejected in ([x for x in events if x.get("Test") != name],
                         [x for x in events if "/" not in x.get("Test", "")],
                         [dict(x, Action="skip") if "/" in x.get("Test", "") and x.get("Action") == "pass" else x for x in events],
                         [dict(x, Action="fail") if "/" in x.get("Test", "") and x.get("Action") == "pass" else x for x in events]):
            with self.subTest(events=rejected), self.assertRaises(RuntimeError):
                RUNNER.verify_results(rejected, {name}, package=RUNNER.BILLING_PACKAGE, require_children=True)

    def test_review_regression_roots_are_required_on_real_postgres(self):
        billing = {
            "TestRefundRejectsPlanBeforeFinancialWritesPostgres",
            "TestRefundSuspendedMemberAdmissionWithoutEarlierBlockerPostgres",
            "TestRefundStalePreflightCannotReclaimUnknownDispatchPostgres",
            "TestPaymentSharedProviderTransactionRejectsOtherOrderAndRepairPostgres",
            "TestRefundExceptionDisprovedHistoryPostgres",
            "TestRefundExceptionHistoryWithoutChargebackRepairFacilityPostgres",
        }
        self.assertTrue(billing <= RUNNER.RETAINED_REQUIRED)
        schema = "TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres"
        self.assertIn(schema, RUNNER.SCHEMA_REQUIRED)
        for required, package in [(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE),
                                  (RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE)]:
            events = self.events(required=required, package=package, children=True)
            RUNNER.verify_results(events, required, package=package)
            for missing in (billing if package == RUNNER.BILLING_PACKAGE else {schema}):
                with self.subTest(missing=missing), self.assertRaises(RuntimeError):
                    RUNNER.verify_results([event for event in events if not event.get("Test", "").startswith(missing)], required, package=package)

    def test_zero_budget_release_and_cancellation_roots_are_selected_and_fail_closed(self):
        for root, required, package, children in [
            ("TestPersonalZeroBudgetHoldReleasePostgres", RUNNER.RETAINED_REQUIRED,
             RUNNER.BILLING_PACKAGE, RUNNER.BILLING_CHILD_REQUIRED),
            ("TestPersonalZeroBudgetCancellationAndExpiryPostgres", RUNNER.REQUIRED,
             RUNNER.TOKEN_PACKAGE, RUNNER.TOKEN_CHILD_REQUIRED),
        ]:
            self.assertIn(root, required)
            self.assertIn(root, children)
            events = self.events(required, package, children=True)
            options = {"package": package, "exact_roots": True,
                       "require_children": True, "children_required": children}
            RUNNER.verify_results(events, required, **options)
            for invalid in [
                [event for event in events if event.get("Test", "").split("/", 1)[0] != root],
                [event for event in events if not event.get("Test", "").startswith(root + "/")],
                events + [{"Action": "skip", "Package": package, "Test": root}],
                events + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
            ]:
                with self.subTest(root=root), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, required, **options)


    def test_payment_instruction_projection_requires_actual_root_and_children(self):
        root = "TestPaymentInstructionProjectionPostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(root, command[command.index("-run") + 1])
        valid = self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        RUNNER.verify_results(valid, RUNNER.RETAINED_REQUIRED, **options)
        for invalid in (
            [],
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
            [event for event in valid if not event.get("Test", "").startswith(root + "/")],
            valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
            valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
            valid + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, RUNNER.RETAINED_REQUIRED, **options)


    def test_organization_chargeback_deletion_evidence_is_required_and_cannot_skip(self):
        self.run_main_fixture()
        root = "TestOrganizationChargebackDeletionEvidencePostgres"
        roots, package = RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE
        self.assertIn(root, roots)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(root, command[command.index("-run") + 1])
        valid = self.events(roots, package, children=True)
        options = {"package": package, "require_children": True, "exact_roots": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        self.assertGreater(RUNNER.verify_results(valid, roots, **options), 0)
        for invalid in (
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
            [event for event in valid if not event.get("Test", "").startswith(root + "/")],
            valid + [{"Action": "skip", "Package": package, "Test": root}],
            valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in valid],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


    def test_recharge_product_atomic_commands_root_and_children_cannot_skip(self):
        name = "TestRechargeProductCommandAtomicityPostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {name}}
        RUNNER.verify_results(events, {name}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != name],
                        [event for event in events if not event.get("Test", "").startswith(name + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)


    def test_negative_adjustment_member_bucket_root_and_children_are_required(self):
        root = "TestNegativeAdjustmentMemberBucketConservationPostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {root}}
        events = self.events(required={root}, package=RUNNER.BILLING_PACKAGE, children=True)
        RUNNER.verify_results(events, {root}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != root],
                        [event for event in events if not event.get("Test", "").startswith(root + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/case"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/case"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {root}, **options)


    def test_product_admission_retirement_binding_root_and_children_cannot_skip(self):
        name = "TestProductAdmissionRetirementBindingPostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {name}}
        RUNNER.verify_results(events, {name}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != name],
                        [event for event in events if not event.get("Test", "").startswith(name + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)


    def test_product_resume_default_provider_root_and_children_cannot_skip(self):
        name = "TestProductResumeDefaultProviderPostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {name}}
        RUNNER.verify_results(events, {name}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != name],
                        [event for event in events if not event.get("Test", "").startswith(name + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)


    def test_provider_blind_command_receipt_root_and_children_cannot_skip(self):
        name = "TestProviderBlindCommandReceiptPostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {name}}
        RUNNER.verify_results(events, {name}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != name],
                        [event for event in events if not event.get("Test", "").startswith(name + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)

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

    def test_manual_admission_cli_selection_and_completed_cases_fail_closed(self):
        manual = "TestProductManualAdmissionPostgres"
        expiry = "TestExpirePendingManualProductAdmissionsReleasesFingerprintSlot"
        materials = {
            "TestProductAdmissionMaterialStorePostgres",
            "TestProductAdmissionMaterialPurgePostgres",
            "TestProductAdmissionMaterialHTTPPostgres",
        }
        roots = {manual, expiry, *materials}
        self.assertEqual(RUNNER.BILLING_LEAF_REQUIRED & roots, {expiry, *materials})
        self.run_main_fixture()
        command = self.fixture_commands[-1]
        selected = re.compile(command[command.index("-run") + 1])
        for root in roots:
            self.assertIsNotNone(selected.fullmatch(root))
        self.assertEqual((RUNNER.billing_roots("billing-0") | RUNNER.billing_roots("billing-1")) & roots, roots)
        self.assertFalse(RUNNER.billing_roots("billing-0") & RUNNER.billing_roots("billing-1"))
        cases = {
            "two-explicit-humans-first-publish-and-immutable-replay",
            "runtime-cannot-rewrite-frozen-manual-facts",
            "concurrent-second-votes-issue-once-and-audit-failure-rolls-back",
            "reject-retains-original-and-no-evidence",
            "stale-product-and-foreign-original-rejected",
            "retention-purges-submission-snapshot-and-review-reason",
        }
        events = []
        for root in sorted(roots):
            children = cases if root == manual else set()
            events.append({"Action": "run", "Package": RUNNER.BILLING_PACKAGE, "Test": root})
            for child in sorted(children):
                for action in ("run", "pass"):
                    events.append({"Action": action, "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/" + child})
            events.append({"Action": "pass", "Package": RUNNER.BILLING_PACKAGE, "Test": root})
        events.append({"Action": "pass", "Package": RUNNER.BILLING_PACKAGE})
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {manual}}
        self.assertEqual(RUNNER.verify_results(events, roots, **options), 11)
        for missing in [*roots, *(manual + "/" + child for child in cases)]:
            incomplete = [event for event in events if event.get("Test") != missing
                          and not event.get("Test", "").startswith(missing + "/")]
            with self.subTest(missing=missing), self.assertRaises(RuntimeError):
                RUNNER.verify_results(incomplete, roots, **options)
        for root in roots:
            for action in ("skip", "fail"):
                rejected = events + [{"Action": action, "Package": RUNNER.BILLING_PACKAGE,
                                      "Test": root}]
                with self.subTest(root=root, action=action), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(rejected, roots, **options)
        skipped = events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE,
                             "Test": manual + "/retention-purges-submission-snapshot-and-review-reason"}]
        with self.assertRaises(RuntimeError):
            RUNNER.verify_results(skipped, roots, **options)

    def events(self, required=None, package=None, children=False):
        result = []
        for name in RUNNER.REQUIRED if required is None else required:
            result += [{"Action": "run", "Package": package, "Test": name}]
            if children:
                result += [{"Action": "run", "Package": package, "Test": name + "/actual"},
                           {"Action": "pass", "Package": package, "Test": name + "/actual"}]
            for child in RUNNER.EXACT_REQUIRED_CHILDREN.get(name, ()):
                result += [{"Action": "run", "Package": package, "Test": name + "/" + child},
                           {"Action": "pass", "Package": package, "Test": name + "/" + child}]
            result += [{"Action": "pass", "Package": package, "Test": name}]
        if package is not None:
            result.append({"Action": "pass", "Package": package})
        return result

    def test_actual_required_suites(self):
        self.assertEqual(RUNNER.verify_results(self.events()), len(RUNNER.REQUIRED) + sum(len(RUNNER.EXACT_REQUIRED_CHILDREN.get(root, ())) for root in RUNNER.REQUIRED))

    def test_context_and_reconciliation_storage_regressions_are_required(self):
        required = {
            "TestContextEligibilityUsesCurrentScopedProductionRoutes",
            "TestOrganizationOriginalOwnerCancellationPublicPostgres",
    "TestRuntimeProtocolPublicationPublicPostgres",
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
            len(RUNNER.REQUIRED) + sum(len(RUNNER.EXACT_REQUIRED_CHILDREN.get(root, ())) for root in RUNNER.REQUIRED),
        )

    def run_main_fixture(self, *, group="all", existing_roles=False, user_objects=False, leftover=False,
                         owner_leftover=False, account_leftover=False, schema_leftover=False, provisioning_leftover=False, active_client=False, external_dependency=False,
                         go_fail_phase=None, empty_phase=None, skipped_phase=None, missing_token_children=False, platform_role_evidence=None):
        writes, commands, phases, trace = [], [], [], []
        state = {"bootstrapped": False, "account_done": False, "owner_done": False, "schema_done": False, "provisioning_done": False, "retained_done": False}

        def run(argv, **kwargs):
            if argv[0] == "go":
                commands.append(argv)
                if argv[-1] == "./internal/account" and "-run" in argv and argv[argv.index("-run") + 1] == "^TestLifecycleV3ApprovalReconciliationPostgres$":
                    phase, required, package = "lifecycle-reconciliation", RUNNER.LIFECYCLE_RECONCILIATION_REQUIRED, RUNNER.ACCOUNT_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                elif argv[-1] == "./internal/account":
                    phase, required, package = "owner-account", RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["account_done"] = True
                elif argv[-1] == "./internal/billing" and not state["bootstrapped"]:
                    phase, required, package = "owner-wallet", RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["owner_done"] = True
                elif argv[-1] == "./internal/token" and not state["bootstrapped"]:
                    phase, required, package = "owner-provisioning", RUNNER.PROVISIONING_REQUIRED, RUNNER.TOKEN_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["provisioning_done"] = True
                elif argv[-1] == "./internal/token":
                    phase, required, package = "retained-token", RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE
                    self.assertTrue(state["bootstrapped"])
                    state["retained_done"] = True
                elif argv[-1] == "./internal/tokenschema":
                    phase, required, package = "owner-token-schema", RUNNER.TOKEN_SCHEMA_REQUIRED, RUNNER.TOKEN_SCHEMA_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                elif argv[-1] == "./internal/billingschema":
                    phase, required, package = "owner-schema", RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE
                    self.assertFalse(state["bootstrapped"])
                    state["schema_done"] = True
                else:
                    phase, required, package = "retained-billing", RUNNER.billing_roots(group), RUNNER.BILLING_PACKAGE
                    self.assertTrue(state["bootstrapped"])
                    state["retained_done"] = True
                phases.append(phase)
                trace.append(phase)
                if phase == "owner-schema":
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_SCHEMA_DATABASE_URL"], "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable")
                    self.assertNotIn("PINVOU_TEST_BILLING_DATABASE_URL", kwargs["env"])
                else:
                    self.assertNotIn("PINVOU_TEST_SCHEMA_DATABASE_URL", kwargs["env"])
                self.assertIn("-race", argv)
                self.assertEqual(kwargs["env"]["GOFLAGS"], "-trimpath -race -tags=nomsgpack -mod=readonly")
                if phase == "owner-account":
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_ACCOUNT_DATABASE_URL"], "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable")
                    self.assertNotIn("PINVOU_TEST_BILLING_DATABASE_URL", kwargs["env"])
                elif phase in {"retained-token", "lifecycle-reconciliation"}:
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_ACCOUNT_DATABASE_URL"], kwargs["env"]["PINVOU_TEST_BILLING_DATABASE_URL"])
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_TOKEN_DATABASE_URL"], kwargs["env"]["PINVOU_TEST_ACCOUNT_DATABASE_URL"])
                elif phase not in {"owner-schema", "owner-token-schema"}:
                    self.assertNotIn("PINVOU_TEST_ACCOUNT_DATABASE_URL", kwargs["env"])
                    self.assertEqual(kwargs["env"]["PINVOU_TEST_BILLING_DATABASE_URL"], "postgres://postgres@127.0.0.1:5433/postgres?sslmode=disable")
                if phase == "owner-schema":
                    self.assertNotIn("PINVOU_TEST_ACCOUNT_DATABASE_URL", kwargs["env"])
                if phase in {"owner-token-schema", "owner-provisioning", "retained-token", "lifecycle-reconciliation"}:
                    self.assertIn("PINVOU_TEST_TOKEN_DATABASE_URL", kwargs["env"])
                else:
                    self.assertNotIn("PINVOU_TEST_TOKEN_DATABASE_URL", kwargs["env"])
                events = self.events(required, package, children=phase != "retained-token")
                if phase == "owner-account":
                    events = self.account_checkpoint_shape(events)
                if phase == "retained-token" and missing_token_children:
                    events = [event for event in events if "/" not in event.get("Test", "")]
                if phase == "retained-token" and not missing_token_children:
                    for name in RUNNER.TOKEN_CHILD_REQUIRED:
                        events += [{"Action": "run", "Package": package, "Test": name + "/actual"},
                                   {"Action": "pass", "Package": package, "Test": name + "/actual"}]
                        for child in RUNNER.EXACT_REQUIRED_CHILDREN.get(name, ()):
                            events += [{"Action": "run", "Package": package, "Test": name + "/" + child},
                                       {"Action": "pass", "Package": package, "Test": name + "/" + child}]
                if phase == "owner-account" and platform_role_evidence:
                    name = "TestPlatformRoleManagementRuntimePostgres"
                    if platform_role_evidence == "missing-root":
                        events = [event for event in events if event.get("Test", "").split("/", 1)[0] != name]
                    elif platform_role_evidence == "missing-children":
                        events = [event for event in events if not event.get("Test", "").startswith(name + "/")]
                    elif platform_role_evidence in {"skipped-root", "skipped-child"}:
                        events.append({"Action": "skip", "Package": package, "Test": name + ("/actual" if platform_role_evidence == "skipped-child" else "")})
                    else:
                        raise AssertionError("unknown platform role evidence mutation")
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
                elif schema_leftover and state["schema_done"]:
                    value += "\nbilling_schema_owner"
                elif provisioning_leftover and state["provisioning_done"]:
                    value += "\ntoken_gateway"
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
            "GOFLAGS": "-trimpath",
        }, clear=True), patch.object(RUNNER.subprocess, "run", side_effect=run), contextlib.redirect_stdout(io.StringIO()):
            try:
                RUNNER.main(group=group)
            finally:
                self.fixture_writes = writes
                self.fixture_commands = commands
                self.fixture_phases = phases
                self.fixture_trace = trace

    def test_parallel_groups_preserve_complete_contract_and_exclusive_cleanup(self):
        phases = []
        shards = []
        for group in RUNNER.GROUPS:
            self.run_main_fixture(group=group)
            phases.extend(self.fixture_phases)
            if group == "owners":
                self.assertEqual(self.fixture_writes, [])
            else:
                self.assertEqual(self.fixture_writes, ["create", "drop"])
            if group.startswith("billing-"):
                command = self.fixture_commands[0]
                roots = RUNNER.billing_roots(group)
                self.assertTrue(roots)
                for root in RUNNER.RETAINED_REQUIRED:
                    self.assertEqual(bool(re.fullmatch(command[command.index("-run") + 1], root)), root in roots)
                shards.append(roots)
            for command in self.fixture_commands:
                self.assertIn("-race", command)
                self.assertIn("-count=1", command)
        self.assertEqual(len(phases), 9)  # Public Token schema phase; Billing is split in two.
        self.assertEqual(set(phases), {"owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "retained-token", "retained-billing"})
        self.assertFalse(shards[0] & shards[1])
        self.assertEqual(shards[0] | shards[1], RUNNER.RETAINED_REQUIRED)
        for group in RUNNER.GROUPS:
            phase = "owner-account" if group == "owners" else "retained-token" if group == "token" else "retained-billing"
            for option in ("empty_phase", "skipped_phase", "go_fail_phase"):
                with self.subTest(group=group, option=option), self.assertRaises(RuntimeError):
                    self.run_main_fixture(group=group, **{option: phase})
            with self.assertRaises(RuntimeError):
                self.run_main_fixture(group=group, existing_roles=True)
            self.assertEqual(self.fixture_commands, [])
        with patch.object(RUNNER.subprocess, "run") as run, self.assertRaises(ValueError):
            RUNNER.main(group="billing-2")
        run.assert_not_called()

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
        self.assertEqual(self.fixture_phases, ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "retained-token"])

    def test_public_entry_executes_account_then_owner_token_and_retained(self):
        self.run_main_fixture()
        self.assertEqual(len(self.fixture_commands), 8)
        command = self.fixture_commands[1]
        selection = command[command.index("-run") + 1]
        self.assertIn("TestF06OrganizationWalletRequiresLifecycleEvidence", selection)
        self.assertIn("TestTokenWalletReceiptSignedClientRecoveryPostgres", selection)
        self.assertIn("TestTokenWalletReceiptConservationAndLifecyclePostgres", selection)
        self.assertEqual(self.fixture_commands[0][-1], "./internal/account")
        self.assertEqual(self.fixture_commands[2][-1], "./internal/billingschema")
        self.assertEqual(self.fixture_commands[3][-1], "./internal/tokenschema")
        self.assertEqual(self.fixture_commands[4][-1], "./internal/token")
        self.assertEqual(self.fixture_commands[5][-1], "./internal/account")
        self.assertEqual(self.fixture_commands[6][-1], "./internal/token")
        self.assertEqual(self.fixture_commands[7][-1], "./internal/billing")

    def test_schema_restoration_is_required_before_retained_bootstrap(self):
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(schema_leftover=True)
        self.assertEqual(self.fixture_writes, [])
        self.assertEqual(self.fixture_phases, ["owner-account", "owner-wallet", "owner-schema"])

    def test_unknown_provider_api_and_append_migration_roots_are_mandatory(self):
        self.run_main_fixture()
        for phase, root, roots, package in (
            ("owner-wallet", "TestReconciliationUnknownProviderEntryPublicPostgres", RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE),
            ("owner-schema", "TestUnknownProviderEntryPublicMigrationPostgres", RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE),
        ):
            self.assertIn(root, roots)
            command = self.fixture_commands[self.fixture_phases.index(phase)]
            self.assertIn(root, command[command.index("-run") + 1])
            valid = self.events(roots, package, children=True)
            options = {"package": package, "require_children": True, "exact_roots": True}
            self.assertGreater(RUNNER.verify_results(valid, roots, **options), 0)
            for invalid in (
                [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
                [event for event in valid if not event.get("Test", "").startswith(root + "/")],
                valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
                [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in valid],
            ):
                with self.subTest(root=root, invalid=invalid), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, roots, **options)

    def test_package_index_schema_contract_is_selected_and_cannot_skip(self):
        self.run_main_fixture()
        roots, package = RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE
        self.assertEqual(roots, {"TestLatePaymentQueryRegisteredIdentity", "TestReconciliationFairRetryRegisteredIdentity", "TestF09EntitlementPackageIndexPublicMigration", "TestTokenWalletReceiptSchemaPostgres", "TestLegacyEntitlementUpgradeAdmissionPostgres", "TestUsageWaiverHistoricalSchemaPostgres", "TestBillingOrdinaryReplayOriginalMigrationPostgres", "TestUnknownProviderEntryPublicMigrationPostgres", "TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres", "TestF04PublicTokenUserOwnerAuthorityAndDirectory", "TestManualRetentionHistoricalHardeningPostgres", "TestBillingHistoricalCatalogRollbackPostgres"})
        command = self.fixture_commands[self.fixture_phases.index("owner-schema")]
        selection = command[command.index("-run") + 1]
        self.assertIn("TestF09EntitlementPackageIndexPublicMigration", selection)
        self.assertIn("TestTokenWalletReceiptSchemaPostgres", selection)
        self.assertIn("TestLegacyEntitlementUpgradeAdmissionPostgres", selection)
        self.assertIn("TestUsageWaiverHistoricalSchemaPostgres", selection)
        self.assertIn("TestBillingOrdinaryReplayOriginalMigrationPostgres", selection)
        self.assertIn("TestF04PublicTokenUserOwnerAuthorityAndDirectory", selection)
        valid = self.events(roots, package, children=True)
        self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
        owner_root = "TestF04PublicTokenUserOwnerAuthorityAndDirectory"
        for invalid in (
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != owner_root],
            [event for event in valid if not event.get("Test", "").startswith(owner_root + "/")],
            valid + [{"Action": "skip", "Package": package, "Test": owner_root}],
            valid + [{"Action": "skip", "Package": package, "Test": owner_root + "/actual"}],
        ):
            with self.subTest(owner_root=owner_root, invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)
        for invalid in (
            [event for event in valid if "/" not in event.get("Test", "")],
            [event for event in valid if event.get("Test")],
            [{**event, "Package": RUNNER.BILLING_PACKAGE} for event in valid],
            valid + [{"Action": "skip", "Package": package, "Test": next(iter(roots)) + "/actual"}],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

    def test_ordinary_billing_replay_root_is_selected_and_requires_real_children(self):
        self.run_main_fixture()
        root = "TestBillingOrdinaryReplayOriginalMigrationPostgres"
        roots, package = RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE
        self.assertIn(root, roots)
        command = self.fixture_commands[self.fixture_phases.index("owner-schema")]
        self.assertIn(root, command[command.index("-run") + 1])
        valid = self.events(roots, package, children=True)
        self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
        for invalid in (
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
            [event for event in valid if not event.get("Test", "").startswith(root + "/")],
            valid + [{"Action": "skip", "Package": package, "Test": root}],
            valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.BILLING_PACKAGE} if event.get("Test", "").split("/", 1)[0] == root else event for event in valid],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

    def test_pre016_admission_root_is_real_scoped_and_cannot_skip(self):
        root = "TestLegacyEntitlementUpgradeAdmissionPostgres"
        roots, package = RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE
        self.assertIn(root, roots)
        valid = self.events(roots, package, children=True)
        self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
        for invalid in (
            [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
            [event for event in valid if not event.get("Test", "").startswith(root + "/")],
            valid + [{"Action": "skip", "Package": package, "Test": root}],
            valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.BILLING_PACKAGE} if event.get("Test", "").split("/", 1)[0] == root else event for event in valid],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

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

    def test_account_phase_requires_real_cas_lifecycle_and_review_governance_roots(self):
        self.assertEqual(RUNNER.ACCOUNT_REQUIRED, {
            "TestRestoredAdminReadsWithPublicMigrationsAndRuntimeActor",
            "TestMemberUnitsReceiptSurvivesLegalUnitDeletion",
            "TestF06OrganizationStatusProjectionVersionPublicPostgres",
            "TestF06OrganizationDeletionVersionAndTerminalScope",
            "TestF06Review5566377593PublicPostgres",
            "TestOrdinaryDeadLetterReplayRuntimePostgres",
            "TestIdentityRecoveryClaimsRuntimePostgres",
            "TestProfileSessionRevocationRecoveryRuntimePostgres",
            "TestRegistrationCreateDispatchRuntimePostgres",
    "TestRegistrationUnknownInspectionPostgres",
            "TestRegistrationIdentityRejectionRuntimePostgres",
            "TestRegistrationCompensationDatabaseErrorIdentityPostgres",
            "TestPlatformRoleManagementRuntimePostgres",
            "TestUserCodeLookupRuntimePostgres",
            "TestAuditSearchOriginalMigrationRuntimePostgres",
            "TestDeliveredPlatformPermissionsPostgres",
            "TestTokenApprovalRecentAuthenticationPostgres",
            "TestTokenApprovalRecentAuthenticationLegacyWriterPostgres",
            "TestTokenAuthorizationWriterToBillingNonTargetPostgres",
            "TestOwnerCombinedActualHTTPPostgres",
            "TestApprovalRecoveryQueryAllRegisteredCommandsPostgres",
            "TestApprovalRecoveryQueryForwardMigrationPostgres",
            "TestApprovalAuthorizationWallClockPostgres",
            "TestApprovalMutationWallClockPostgres",
            "TestApprovalProofWallClockPostgres",
            "TestApprovalWithdrawalOriginalReceiptPostgres",
            "TestApprovalDecisionOriginalReceiptPostgres",
            "TestApprovalDecisionLegacyConcurrentReceiptPostgres",
            "TestOrganizationUnitAuthorityWallClockPostgres",
            "TestApprovalListPoolRuntimePostgres",
            "TestPasswordPreparationAuthorizationPostgres",
            "TestRegistrationBonusEligibilityCheckpointPostgresIntegration",
            "TestRegistrationBonusCheckpointGuardsPostgresIntegration",
            "TestRegistrationBonusCheckpointMigrationPostgresIntegration",
            "TestRegistrationBonusCheckpointForeignFactsPostgresIntegration",
            "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration",
            "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration",
        })
        for name in RUNNER.ACCOUNT_REQUIRED:
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                RUNNER.verify_results([e for e in self.events(RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE, True) if e.get("Test", "").split("/")[0] != name], RUNNER.ACCOUNT_REQUIRED, package=RUNNER.ACCOUNT_PACKAGE, require_children=True, exact_roots=True)

    def test_account_roots_are_selected_with_real_children(self):
        self.run_main_fixture()
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        valid = self.events(roots, package, children=True)
        for root in (
            "TestUserCodeLookupRuntimePostgres",
            "TestAuditSearchOriginalMigrationRuntimePostgres",
            "TestDeliveredPlatformPermissionsPostgres",
            "TestTokenApprovalRecentAuthenticationPostgres",
            "TestTokenApprovalRecentAuthenticationLegacyWriterPostgres",
            "TestTokenAuthorizationWriterToBillingNonTargetPostgres",
            "TestOwnerCombinedActualHTTPPostgres",
            "TestApprovalRecoveryQueryAllRegisteredCommandsPostgres",
            "TestApprovalRecoveryQueryForwardMigrationPostgres",
            "TestApprovalAuthorizationWallClockPostgres",
            "TestApprovalMutationWallClockPostgres",
            "TestApprovalProofWallClockPostgres",
            "TestApprovalWithdrawalOriginalReceiptPostgres",
            "TestApprovalDecisionOriginalReceiptPostgres",
            "TestApprovalDecisionLegacyConcurrentReceiptPostgres",
            "TestOrganizationUnitAuthorityWallClockPostgres",
            "TestApprovalListPoolRuntimePostgres",
            "TestPasswordPreparationAuthorizationPostgres",
        ):
            with self.subTest(root=root):
                self.assertIn(root, roots)
                self.assertIn(root, command[command.index("-run") + 1])
                self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
                for invalid in (
                    [event for event in valid if event.get("Test", "").split("/", 1)[0] != root],
                    [event for event in valid if not event.get("Test", "").startswith(root + "/")],
                    valid + [{"Action": "skip", "Package": package, "Test": root}],
                    valid + [{"Action": "skip", "Package": package, "Test": root + "/actual"}],
                    [{**event, "Package": RUNNER.BILLING_PACKAGE} if event.get("Test", "").split("/", 1)[0] == root else event for event in valid],
                ):
                    with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                        RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

    def test_review_governance_and_batch_are_selected_with_mandatory_child_evidence(self):
        required = {
            "owner-account": ("TestF06Review5566377593PublicPostgres", RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE),
            "retained-billing": ("TestMemberQuotaBatchPersistedFactsAndFinancialReplay", RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE),
        }
        self.run_main_fixture()
        for phase, (name, roots, package) in required.items():
            with self.subTest(phase=phase):
                command = self.fixture_commands[self.fixture_phases.index(phase)]
                self.assertIn(name, command[command.index("-run") + 1])
                self.assertIn(name, roots)
                valid = self.events(roots, package, children=True)
                self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
                for missing in (
                    [event for event in valid if event.get("Test", "").split("/")[0] != name],
                    [event for event in valid if not event.get("Test", "").startswith(name + "/")],
                    valid + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
                ):
                    with self.assertRaises(RuntimeError):
                        RUNNER.verify_results(missing, roots, package=package, require_children=True, exact_roots=True)

    def test_actual_main_requires_token_waiver_children(self):
        self.assertEqual(
            RUNNER.TOKEN_CHILD_REQUIRED,
            {"TestEmergencyCancellationClaimOwnershipPostgres", "TestEmergencyCancellationRequestLocksClaimPostgres", "TestUserUnusedIntentCancellationOwnerPostgres", "TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres", "TestPendingUnusedExpiryCancellationOwnerPostgres", "TestPendingUnusedExpiryLegacyCancellationFallbackPostgres", "TestRemoteModelCancellationOwnerLoopTransport", "TestLocalModelCancellationHandshakeHTTP", "TestLifecycleForceCancelApprovalPostgres", "TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres", "TestCancellationReceivedReadRechecksReservationPostgres", "TestCancellationFirstUserPreservesConfirmedReceiptPostgres", "TestModelTerminalObservationPreservesExpiryProofPostgres", "TestModelTerminalObservationRejectsUntrustedEvidencePostgres", "TestUsageWaiverCoordinatorRecoveryPostgres", "TestPersonalZeroBudgetCancellationAndExpiryPostgres", "TestCredentialRotationWindowReceiptPostgres", "TestCredentialFailurePersistenceBoundariesPostgres", "TestCircuitFailureBeforeDispatchPostgres", "TestPreparationFailurePreservesAcceptedUsagePostgres"},
        )
        with self.assertRaisesRegex(RuntimeError, "no completed child"):
            self.run_main_fixture(missing_token_children=True)
        self.assertEqual(self.fixture_phases[-1], "retained-token")

    def test_usage_waiver_roots_require_real_scoped_child_evidence(self):
        self.run_main_fixture()
        for phase, roots, package, required in (
            ("owner-schema", RUNNER.SCHEMA_REQUIRED, RUNNER.SCHEMA_PACKAGE, {"TestUsageWaiverHistoricalSchemaPostgres"}),
            ("retained-token", RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE, {"TestUsageWaiverCoordinatorRecoveryPostgres"}),
            ("retained-billing", RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, {
                "TestUsageWaiverFinancialConservationPostgres", "TestUsageWaiverCommitWinnerPostgres",
                "TestUsageWaiverMemberReservationPostgres", "TestUsageWaiverExpiryRepairWinnerPostgres",
            }),
        ):
            command = self.fixture_commands[self.fixture_phases.index(phase)]
            valid = self.events(roots, package, children=True)
            for name in required:
                with self.subTest(phase=phase, root=name):
                    self.assertIn(name, roots)
                    if phase != "retained-token":
                        self.assertIn(name, command[command.index("-run") + 1])
                    self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True), 0)
                    for invalid in (
                        [event for event in valid if event.get("Test", "").split("/")[0] != name],
                        [event for event in valid if not event.get("Test", "").startswith(name + "/")],
                        valid + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
                    ):
                        with self.assertRaises(RuntimeError):
                            RUNNER.verify_results(invalid, roots, package=package, require_children=True)

    def test_platform_role_root_is_selected_and_requires_real_scoped_children(self):
        name = "TestPlatformRoleManagementRuntimePostgres"
        for evidence in ("missing-root", "missing-children", "skipped-root", "skipped-child"):
            with self.subTest(main_evidence=evidence), self.assertRaises(RuntimeError):
                self.run_main_fixture(platform_role_evidence=evidence)
            self.assertEqual(self.fixture_phases, ["owner-account"])
            self.assertEqual(self.fixture_writes, [])
        self.run_main_fixture()
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, roots)
        self.assertIn(name, command[command.index("-run") + 1])
        self.assertEqual(command[-1], "./internal/account")
        valid = self.events(roots, package, children=True)
        self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
        invalid_cases = {
            "missing-root": [event for event in valid if event.get("Test", "").split("/", 1)[0] != name],
            "missing-children": [event for event in valid if not event.get("Test", "").startswith(name + "/")],
            "skipped-root": valid + [{"Action": "skip", "Package": package, "Test": name}],
            "skipped-child": valid + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            "wrong-package": [{**event, "Package": RUNNER.BILLING_PACKAGE} if event.get("Test", "").split("/", 1)[0] == name else event for event in valid],
        }
        for label, invalid in invalid_cases.items():
            with self.subTest(case=label), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

    def test_registration_dispatch_root_requires_actual_completed_children(self):
        for name in (
            "TestRegistrationCreateDispatchRuntimePostgres",
            "TestRegistrationUnknownInspectionPostgres",
            "TestRegistrationIdentityRejectionRuntimePostgres",
        ):
            roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
            self.run_main_fixture()
            command = self.fixture_commands[self.fixture_phases.index("owner-account")]
            self.assertIn(name, command[command.index("-run") + 1])
            valid = self.events(roots, package, children=True)
            self.assertGreater(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 0)
            for invalid in (
                [event for event in valid if event.get("Test", "").split("/")[0] != name],
                [event for event in valid if not event.get("Test", "").startswith(name + "/")],
                valid + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            ):
                with self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)

    def test_expiry_restore_root_is_selected_and_requires_completed_children(self):
        name = "TestF06MemberQuotaRestoreUsesOrdinaryPackageExpiry"
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(name, command[command.index("-run") + 1])
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        roots, package = RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE
        valid = self.events(roots, package, children=True)
        for missing in (
            [event for event in valid if event.get("Test", "").split("/")[0] != name],
            [event for event in valid if not event.get("Test", "").startswith(name + "/")],
            valid + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
        ):
            with self.assertRaises(RuntimeError):
                RUNNER.verify_results(missing, roots, package=package, require_children=True, exact_roots=True)

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
                                ("owner-schema", ["owner-account", "owner-wallet", "owner-schema"]),
                                ("owner-provisioning", ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning"]),
                                ("lifecycle-reconciliation", ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation"]),
                                ("retained-token", ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "retained-token"]),
                                ("retained-billing", ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "retained-token", "retained-billing"])]:
            with self.subTest(phase=phase), self.assertRaises(RuntimeError):
                self.run_main_fixture(go_fail_phase=phase)
            self.assertEqual(self.fixture_phases, expected)
            self.assertEqual(self.fixture_writes, [] if phase in {"owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation"} else ["create", "drop"])

    def test_each_phase_rejects_zero_or_skipped_required_tests(self):
        for phase in ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "retained-token", "retained-billing"]:
            for option in ["empty_phase", "skipped_phase"]:
                with self.subTest(phase=phase, option=option), self.assertRaises(RuntimeError):
                    self.run_main_fixture(**{option: phase})
                self.assertEqual(self.fixture_phases[-1], phase)

    def test_actual_provisioning_consumer_owns_absent_actors_before_bootstrap(self):
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-provisioning")]
        roots, package = RUNNER.PROVISIONING_REQUIRED, RUNNER.TOKEN_PACKAGE
        self.assertEqual(roots, {"TestTokenWalletSupplierActualProvisioningConsumerPostgres"})
        self.assertEqual(command[command.index("-run") + 1], "^" + next(iter(roots)) + "$")
        self.assertEqual(command[-1], "./internal/token")
        self.assertNotIn("-skip", command)
        retained = self.fixture_commands[self.fixture_phases.index("retained-token")]
        self.assertNotIn("-run", retained)
        self.assertEqual(retained[retained.index("-skip") + 1], "^(" + next(iter(roots)) + ")$")
        self.assertEqual(retained[-1], "./internal/token")
        for phase, argv in zip(self.fixture_phases, self.fixture_commands):
            if phase != "retained-token":
                self.assertNotIn("-skip", argv)
        valid = self.events(roots, package, children=True)
        self.assertEqual(RUNNER.verify_results(valid, roots, package=package, require_children=True, exact_roots=True), 2)
        for invalid in (
            [{"Action": "pass", "Package": package}],
            [event for event in valid if "/" not in event.get("Test", "")],
            [{**event, "Package": RUNNER.ACCOUNT_PACKAGE} for event in valid],
            valid + [{"Action": "skip", "Package": package, "Test": next(iter(roots)) + "/actual"}],
        ):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, package=package, require_children=True, exact_roots=True)
        with self.assertRaises(RuntimeError):
            self.run_main_fixture(provisioning_leftover=True)
        self.assertEqual(self.fixture_writes, [])
        self.assertEqual(self.fixture_phases, ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning"])

    def test_scoped_evidence_requires_exact_package_and_children_and_completion(self):
        valid = self.events(RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        self.assertEqual(RUNNER.verify_results(valid, RUNNER.OWNER_REQUIRED, package=RUNNER.BILLING_PACKAGE, require_children=True, exact_roots=True), 2 * len(RUNNER.OWNER_REQUIRED))
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

    def test_wallet_receipt_roots_cannot_be_omitted_or_skipped(self):
        self.assertEqual(RUNNER.OWNER_REQUIRED, {
            "TestPaymentActionAdmissionPostgres",
            "TestF06OrganizationWalletRequiresLifecycleEvidence",
            "TestTokenWalletReceiptSignedClientRecoveryPostgres",
            "TestTokenWalletReceiptConservationAndLifecyclePostgres",
            "TestOrganizationPlanPurchaseActualClientPostgres",
            "TestOrganizationPlanPurchaseLifecycleFencePostgres",
            "TestBillingLedgerAuditReadOnlyPublicPostgres",
            "TestBillingFinancialEventOutboxContractsPostgres",
            "TestUsageWaiverOriginal828HTTPReplayPostgres",
            "TestReconciliationUnknownProviderEntryPublicPostgres",
        })
        valid = self.events(RUNNER.OWNER_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        for name in RUNNER.OWNER_REQUIRED:
            mutations = [
                [event for event in valid if event.get("Test", "").split("/")[0] != name],
                [event for event in valid if not event.get("Test", "").startswith(name + "/")],
                valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
            ]
            for events in mutations:
                with self.subTest(name=name), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(events, RUNNER.OWNER_REQUIRED, package=RUNNER.BILLING_PACKAGE, require_children=True, exact_roots=True)

    def test_owned_fixture_restores_only_its_actors(self):
        self.run_main_fixture()
        self.assertEqual(self.fixture_writes, ["create", "drop"])
        self.assertEqual(self.fixture_trace, ["owner-account", "owner-wallet", "owner-schema", "owner-token-schema", "owner-provisioning", "lifecycle-reconciliation", "create", "retained-token", "retained-billing", "drop"])

    def test_ledger_activation_read_failure_regression_is_mandatory(self):
        root = "TestLedgerRepairClaimActivationReadErrorsPostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, RUNNER.RETAINED_REQUIRED, **options), 0)
        missing = [event for event in events if event.get("Test", "").split("/", 1)[0] != root]
        no_child = [event for event in events if not event.get("Test", "").startswith(root + "/")]
        wrong_package = [dict(event, Package="unexpected/billing")
                         if event.get("Test", "").split("/", 1)[0] == root else event
                         for event in events]
        skipped = events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}]
        for invalid in (missing, no_child, wrong_package, skipped):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, RUNNER.RETAINED_REQUIRED, **options)

    def test_positive_extension_suites_are_mandatory(self):
        self.assertIn("TestF05AdditionalReservationProjectionPublicPostgres", RUNNER.REQUIRED)
        billing = {"TestHoldExtensionIdempotencePostgres", "TestPersonalUsageHoldBoundaries",
                   "TestPersonalUsageHoldClockBoundaries", "TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved"}
        self.assertTrue(billing <= RUNNER.RETAINED_REQUIRED)
        self.assertIn("TestMemberHoldIdentityMutationBoundaries", RUNNER.RETAINED_REQUIRED)
        for required, package in [(RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE),
                                  (RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE)]:
            for name in required:
                events = [event for event in self.events(required, package) if event.get("Test") != name]
                with self.subTest(name=name), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(events, required, package=package)

    def test_only_explicit_original_and_refund_leaves_can_omit_children(self):
        leaves = {
            "TestExpirePendingManualProductAdmissionsReleasesFingerprintSlot",
            "TestProductAdmissionMaterialStorePostgres",
            "TestProductAdmissionMaterialPurgePostgres",
            "TestProductAdmissionMaterialHTTPPostgres",
            "TestEscalatedForcedRefundRequiresSecondActorAndReplays",
            "TestEscalatedForcedRefundWalletLockOrderPostgres",
            "TestOrdinaryForcedRefundAppliedReceiptRecoveryPostgres",
            "TestPaymentPersonalWalletAdmissionStatusRacePostgres",
            "TestPostgresManagedPricePagination",
            "TestLedgerRepairProofHistoricalUpgradePostgres",
            "TestLedgerRepairAuthorityMixedBatchPostgres",
            "TestLedgerRepairHistoryCapabilityPostgres",
            "TestReconciliationLedgerRepairForcedPeerPostgres",
            "TestReconciliationLedgerRepairPublicPostgres",
            "TestCallbackHTTPAdmissionPersistsPostgres",
            "TestRefundRejectsPlanBeforeFinancialWritesPostgres",
            "TestRefundSuspendedMemberAdmissionWithoutEarlierBlockerPostgres",
            "TestRefundStalePreflightCannotReclaimUnknownDispatchPostgres",
            "TestPaymentSharedProviderTransactionRejectsOtherOrderAndRepairPostgres",
            "TestRefundExceptionHistoryWithoutChargebackRepairFacilityPostgres",
            "TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved",
            "TestRefundRepairRetainsZeroHistoricalCorrectionSourcePostgres",
            "TestRefundOriginalSuccessQueryRetainsCompetingFinancialFreezePostgres",
            "TestRefundNeverDispatchedReviewCannotBecomeNewProviderRequestPostgres",
            "TestRefundUnknownMissingSimulatorResultNeverCreatesRefundPostgres",
            "TestRefundOriginalUnknownSuccessQueriesAndSettlesOrdinaryGrantsPostgres",
        }
        self.assertEqual(RUNNER.BILLING_LEAF_REQUIRED, leaves)
        self.assertEqual(RUNNER.BILLING_CHILD_REQUIRED, RUNNER.RETAINED_REQUIRED - leaves)
        valid = [event for event in self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
                 if event.get("Test", "").split("/", 1)[0] not in leaves or "/" not in event.get("Test", "")]
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        self.assertGreater(RUNNER.verify_results(valid, RUNNER.RETAINED_REQUIRED, **options), 0)
        for name in RUNNER.BILLING_CHILD_REQUIRED:
            without_child = [event for event in valid if not event.get("Test", "").startswith(name + "/")]
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                RUNNER.verify_results(without_child, RUNNER.RETAINED_REQUIRED, **options)
        for leaf in leaves:
            for name in [leaf, leaf + "/unexpected"]:
                with self.subTest(name=name), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                                          RUNNER.RETAINED_REQUIRED, **options)

    def test_all_refund_fund_paths_are_selected_and_each_missing_or_skipped_root_is_rejected(self):
        expected = {
            "TestRefundRejectsPlanBeforeFinancialWritesPostgres",
            "TestRefundSuspendedMemberAdmissionWithoutEarlierBlockerPostgres",
            "TestRefundStalePreflightCannotReclaimUnknownDispatchPostgres",
            "TestPaymentSharedProviderTransactionRejectsOtherOrderAndRepairPostgres",
            "TestRefundExceptionHistoryWithoutChargebackRepairFacilityPostgres",
            "TestRefundExceptionDisprovedHistoryPostgres",
            "TestRefundPersistedSuccessReferenceUsesPublishedCanonicalIdentityPostgres",
            "TestReviewRefundReconcileRequiresTerminalProviderEvidence",
            "TestReviewRefundReconcileRejectsMismatchedEvidence",
            "TestRefundRejectsExistingSuspendedGrantsBeforeReservationPostgres",
            "TestRefundLegacySuspendedDispatchNeverCallsProviderPostgres",
            "TestRefundVerifiedSuccessRepairsOriginalSuspendedGrantPostgres",
            "TestRefundRepairRetainsZeroHistoricalCorrectionSourcePostgres",
            "TestRefundCorrectedZeroSourceAndPriorSuspensionRecoverTogetherPostgres",
            "TestRefundPriorSuspensionRepairFrozenGrantChangesFailWithoutEffectsPostgres",
            "TestRefundUnknownQueryCannotInventSuccessOrDispatchPostgres",
            "TestRefundOriginalSuccessQueryRetainsCompetingFinancialFreezePostgres",
            "TestRefundNeverDispatchedReviewCannotBecomeNewProviderRequestPostgres",
            "TestRefundLegacyUnknownSuccessQueriesOriginalSimulatorThenRepairsPostgres",
            "TestRefundUnknownMissingSimulatorResultNeverCreatesRefundPostgres",
            "TestRefundOriginalUnknownSuccessQueriesAndSettlesOrdinaryGrantsPostgres",
            "TestRefundProviderNetworkOutsideFinancialTransactionPostgres",
        }
        self.assertEqual(RUNNER.REFUND_REQUIRED, expected)
        self.assertTrue(expected <= RUNNER.RETAINED_REQUIRED)
        self.assertEqual(len(RUNNER.RETAINED_REQUIRED), 75)
        valid = self.events(RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True, "exact_roots": True,
                   "children_required": RUNNER.BILLING_CHILD_REQUIRED}
        for root in expected:
            missing = [event for event in valid if event.get("Test", "").split("/", 1)[0] != root]
            for invalid in [missing, valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
                            valid + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}]]:
                with self.subTest(root=root), self.assertRaises(RuntimeError):
                    RUNNER.verify_results(invalid, RUNNER.RETAINED_REQUIRED, **options)
    def test_required_suites_cannot_be_supplied_by_another_package(self):
        for required, package in [(RUNNER.REQUIRED, RUNNER.TOKEN_PACKAGE),
                                  (RUNNER.RETAINED_REQUIRED, RUNNER.BILLING_PACKAGE)]:
            wrong = self.events(required, "unrelated/package", children=True)
            with self.subTest(package=package), self.assertRaises(RuntimeError):
                RUNNER.verify_results(wrong, required, package=package)


    def test_late_archive_receipt_root_is_selected_and_requires_completed_children(self):
        root = "TestLatePaymentArchiveCommandReplayPostgres"
        self.assertIn(root, RUNNER.RETAINED_REQUIRED)
        self.assertIn(root, RUNNER.BILLING_CHILD_REQUIRED)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("retained-billing")]
        self.assertIn(root, command[command.index("-run") + 1])
        events = self.events({root}, RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "exact_roots": True,
                   "require_children": True, "children_required": {root}}
        RUNNER.verify_results(events, {root}, **options)
        for invalid in (
            [],
            [event for event in events if event.get("Test") != root],
            [event for event in events if "/" not in event.get("Test", "")],
            [event for event in events if not (event.get("Test") == root and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root}],
            events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
            events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": root + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {root}, **options)


    def test_direct_payment_repair_command_receipt_root_and_children_cannot_skip(self):
        name = "TestDirectPaymentRepairCommandReceiptPostgres"
        self.assertIn(name, RUNNER.RETAINED_REQUIRED)
        self.assertIn(name, RUNNER.BILLING_CHILD_REQUIRED)
        events = self.events(required={name}, package=RUNNER.BILLING_PACKAGE, children=True)
        options = {"package": RUNNER.BILLING_PACKAGE, "require_children": True,
                   "exact_roots": True, "children_required": {name}}
        RUNNER.verify_results(events, {name}, **options)
        for invalid in ([],
                        [event for event in events if event.get("Test") != name],
                        [event for event in events if not event.get("Test", "").startswith(name + "/")],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name}],
                        events + [{"Action": "skip", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}],
                        events + [{"Action": "fail", "Package": RUNNER.BILLING_PACKAGE, "Test": name + "/actual"}]):
            with self.subTest(events=invalid), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, {name}, **options)


    def test_decision_legacy_concurrent_receipt_is_selected_and_requires_real_children(self):
        name = "TestApprovalDecisionLegacyConcurrentReceiptPostgres"
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        self.assertIn(name, roots)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, command[command.index("-run") + 1])
        events = self.events(roots, package, children=True)
        options = {"package": package, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, roots, **options), 0)
        for invalid in (
            [],
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            [event for event in events if not (event.get("Test", "") == name and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": package, "Test": name}],
            events + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            events + [{"Action": "fail", "Package": package, "Test": name + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} if event.get("Test", "").split("/", 1)[0] == name else event for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


    def test_decision_original_receipt_is_selected_and_requires_real_children(self):
        name = "TestApprovalDecisionOriginalReceiptPostgres"
        roots, package = RUNNER.ACCOUNT_REQUIRED, RUNNER.ACCOUNT_PACKAGE
        self.assertIn(name, roots)
        self.run_main_fixture()
        command = self.fixture_commands[self.fixture_phases.index("owner-account")]
        self.assertIn(name, command[command.index("-run") + 1])
        events = self.events(roots, package, children=True)
        options = {"package": package, "exact_roots": True, "require_children": True}
        self.assertGreater(RUNNER.verify_results(events, roots, **options), 0)
        for invalid in (
            [],
            [event for event in events if event.get("Test", "").split("/", 1)[0] != name],
            [event for event in events if not event.get("Test", "").startswith(name + "/")],
            [event for event in events if not (event.get("Test", "") == name and event.get("Action") == "run")],
            events + [{"Action": "skip", "Package": package, "Test": name}],
            events + [{"Action": "skip", "Package": package, "Test": name + "/actual"}],
            events + [{"Action": "fail", "Package": package, "Test": name + "/actual"}],
            [{**event, "Package": RUNNER.TOKEN_PACKAGE} if event.get("Test", "").split("/", 1)[0] == name else event for event in events],
        ):
            with self.subTest(events=len(invalid)), self.assertRaises(RuntimeError):
                RUNNER.verify_results(invalid, roots, **options)


if __name__ == "__main__":
    unittest.main()
