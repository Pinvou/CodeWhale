#!/usr/bin/env python3
"""Run existing Token and scoped organization financial contracts on disposable PG.

The Account fixture first owns its eight absent actors, then wallet
evidence and provisioning fixtures own their absent actors exclusively.
After strict restoration, retained fixtures create their own UUID databases and
Token actors while this runner owns the three Billing deployment actors. Only
the three Billing deployment actors are bootstrapped here, using their existing
privilege contract. This runner must never accept a deployed database URL.
"""
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[1]
ACTORS = {"billing_migrator", "billing_schema_owner", "billing_service"}
REQUIRED = {
    'TestLocalModelCancellationHandshakeHTTP',
    'TestRemoteModelCancellationOwnerLoopTransport',
    "TestPreparationFailurePreservesAcceptedUsagePostgres",
    "TestCircuitFailureBeforeDispatchPostgres",
    "TestCredentialRotationWindowReceiptPostgres",
    "TestCredentialRotationOtherActorEffectPostgres",
    "TestCredentialFailurePersistenceBoundariesPostgres",
    "TestCredentialFailurePreparationFencePostgres",
    "TestCredentialFailureExistingEventsPostgres",
    "TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres",
    "TestPersonalZeroBudgetCancellationAndExpiryPostgres",
    "TestUsageWaiverCoordinatorRecoveryPostgres",
    "TestF05AdditionalReservationProjectionPublicPostgres",
    "TestF06OrganizationAdmissionBoundariesPublicPostgres",
    "TestContextEligibilityUsesCurrentScopedProductionRoutes",
    "TestReconciliationEvidenceMatchesPublicSchema",
    "TestF05DispatchLeaseLifecycle",
    "TestF05IncompleteUpstreamErrorReplayPublicPostgres",
    "TestF05MissingReferenceDeadlineFundsPublicPostgres",
    "TestF05CancellationKeyScopePublicPostgres",
    "TestF05CancellationRecoveryReconciliationOwnership",
    "TestF05CancellationRecoveryClosurePublicPostgres",
    "TestF05RotationRetainedContentBoundaries",
    "TestF05UnsettledContentRetention",
    "TestF05ExpiredUnacquiredDispatchRecovery",
    "TestF05PersonalHistoryAndCancellationPersistedOwnerScope",
    "TestOrganizationOriginalOwnerCancellationPublicPostgres",
    "TestRuntimeProtocolPublicationPublicPostgres",
    "TestF05AdminUsageGateOffUsesSafeCollectionProjection",
    "TestPersonalExecutionSameKeyReplaysAtInflightLimit",
    "TestPersonalExecutionPublicPostgresFundsAndReplay",
    "TestManagementCancellationFundsRemainTruthful",
    "TestManagementEmergencyCancellationRecoversLostReserveACK",
    "TestManagementCancellationHoldRecoveryRequiresRecordedProof",
}

ACCOUNT_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/account"
ACCOUNT_REQUIRED = {
    "TestApprovalDecisionOriginalReceiptPostgres",
    "TestApprovalDecisionLegacyConcurrentReceiptPostgres",
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
    "TestOrganizationUnitAuthorityWallClockPostgres",
    "TestApprovalListPoolRuntimePostgres",
    "TestPasswordPreparationAuthorizationPostgres",
    "TestApprovalWithdrawalOriginalReceiptPostgres",
}
TOKEN_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/token"
TOKEN_CHILD_REQUIRED = {
    'TestLocalModelCancellationHandshakeHTTP',
    'TestRemoteModelCancellationOwnerLoopTransport',
    "TestPreparationFailurePreservesAcceptedUsagePostgres",
    "TestCircuitFailureBeforeDispatchPostgres",
    "TestCredentialRotationWindowReceiptPostgres",
    "TestCredentialFailurePersistenceBoundariesPostgres",
    "TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres",
    "TestUsageWaiverCoordinatorRecoveryPostgres",
    "TestPersonalZeroBudgetCancellationAndExpiryPostgres",
}
BILLING_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/billing"
SCHEMA_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
SCHEMA_REQUIRED = {"TestLatePaymentQueryRegisteredIdentity", "TestReconciliationFairRetryRegisteredIdentity", "TestF09EntitlementPackageIndexPublicMigration", "TestTokenWalletReceiptSchemaPostgres", "TestLegacyEntitlementUpgradeAdmissionPostgres", "TestUsageWaiverHistoricalSchemaPostgres", "TestBillingOrdinaryReplayOriginalMigrationPostgres", "TestUnknownProviderEntryPublicMigrationPostgres", "TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres", "TestF04PublicTokenUserOwnerAuthorityAndDirectory"}
OWNER_REQUIRED = {
    "TestF06OrganizationWalletRequiresLifecycleEvidence",
    "TestTokenWalletReceiptSignedClientRecoveryPostgres",
    "TestTokenWalletReceiptConservationAndLifecyclePostgres",
    "TestOrganizationPlanPurchaseActualClientPostgres",
    "TestOrganizationPlanPurchaseLifecycleFencePostgres",
    "TestBillingLedgerAuditReadOnlyPublicPostgres",
    "TestBillingFinancialEventOutboxContractsPostgres",
    "TestUsageWaiverOriginal828HTTPReplayPostgres",
    "TestReconciliationUnknownProviderEntryPublicPostgres",
}
PROVISIONING_REQUIRED = {"TestTokenWalletSupplierActualProvisioningConsumerPostgres"}
REFUND_REQUIRED = {
    "TestRefundProviderNetworkOutsideFinancialTransactionPostgres",
    "TestRefundRejectsPlanBeforeFinancialWritesPostgres",
    "TestRefundSuspendedMemberAdmissionWithoutEarlierBlockerPostgres",
    "TestRefundStalePreflightCannotReclaimUnknownDispatchPostgres",
    "TestPaymentSharedProviderTransactionRejectsOtherOrderAndRepairPostgres",
    "TestRefundExceptionDisprovedHistoryPostgres",
    "TestRefundExceptionHistoryWithoutChargebackRepairFacilityPostgres",
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
}
RETAINED_REQUIRED = {
    "TestPostgresManagedPricePagination",
    "TestLedgerRepairProofHistoricalUpgradePostgres",
    "TestLedgerRepairAuthorityDuplicateForcedRefundPostgres",
    "TestLedgerRepairAuthorityCrossDatePostgres",
    "TestLedgerRepairAuthorityMixedBatchPostgres",
    "TestLedgerRepairHistoryCapabilityPostgres",
    "TestF03CumulativeForcedRefundReceiptAudit",
    "TestRealRepairOnRefundAndChargeback",
    "TestReconciliationLedgerRepairForcedPeerPostgres",
    "TestReconciliationLedgerRepairPublicPostgres",
    "TestCallbackHTTPAdmissionPersistsPostgres",
    "TestDirectPaymentRepairCommandReceiptPostgres",
    "TestLatePaymentQueryCommandPostgres",
    "TestLatePaymentQueryClientHTTPPostgres",
    "TestLatePaymentArchiveCommandReplayPostgres",
    "TestProviderBlindCommandReceiptPostgres",
    "TestLatePaymentScheduleCommandReplayPostgres",
    "TestReconciliationFairRetryRuntimePostgres",
    "TestProductResumeDefaultProviderPostgres",
    "TestProductAdmissionRetirementBindingPostgres",
    "TestNegativeAdjustmentMemberBucketConservationPostgres",
    "TestRechargeProductCommandAtomicityPostgres",
    "TestOrganizationChargebackDeletionEvidencePostgres",
    "TestPaymentInstructionProjectionPostgres",
    "TestPersonalZeroBudgetHoldReleasePostgres",
    "TestForcedRefundMemberGrantTerminalStatePostgres",
    "TestUsageWaiverFinancialConservationPostgres",
    "TestUsageWaiverCommitWinnerPostgres",
    "TestUsageWaiverMemberReservationPostgres",
    "TestUsageWaiverExpiryRepairWinnerPostgres",
    *REFUND_REQUIRED,
    "TestLedgerRepairClaimActivationReadErrorsPostgres",
    "TestF06MemberQuotaRestoreUsesOrdinaryPackageExpiry",
    "TestMemberQuotaBatchPersistedFactsAndFinancialReplay",
    "TestMemberHoldIdentityMutationBoundaries",
    "TestPersonalUsageHoldBoundaries",
    "TestHoldExtensionIdempotencePostgres",
    "TestPersonalUsageHoldClockBoundaries",
    "TestF05ExpiredHoldRenewalIsConflictAndUnprovenFundsStayReserved",
}
# These exact roots are named leaves. Every other retained root must still
# produce completed child-case evidence, not just a parent PASS.
BILLING_LEAF_REQUIRED = {
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
BILLING_CHILD_REQUIRED = RETAINED_REQUIRED - BILLING_LEAF_REQUIRED
EXACT_REQUIRED_CHILDREN = {
    'TestLocalModelCancellationHandshakeHTTP': {'after_persistent_read', 'before_registration', 'during_persistent_read', 'emergency_then_user_before_registration', 'normal', 'read_failure_preserves_funds', 'user_stop_preserves_generation'},
    'TestRemoteModelCancellationOwnerLoopTransport': {'initial_lease_failure', 'initial_read_failure', 'initial_requested', 'lease_failure', 'natural_terminal', 'read_failure', 'remote_emergency', 'remote_emergency_then_user', 'user_only'},
    "TestRegistrationCompensationDatabaseErrorIdentityPostgres": {
        "operation-read",
        "absence-proof-read",
        "completion-write",
        "missing-completion-remains-business-rejection",
        "noncompensable-registration-remains-rejected",
    },
    "TestLedgerRepairAuthorityDuplicateForcedRefundPostgres": {
        "reverse=false/available=false",
        "reverse=false/available=true",
        "reverse=true/available=false",
        "reverse=true/available=true",
    },
}


def local_database_url(raw):
    url = urlsplit(raw)
    query = parse_qs(url.query, keep_blank_values=True)
    if (
        url.scheme not in {"postgres", "postgresql"}
        or url.hostname != "127.0.0.1"
        or not url.port
        or not url.username
        or url.password is not None
        or url.path != "/postgres"
        or url.fragment
        or query != {"sslmode": ["disable"]}
    ):
        raise ValueError("requires password-free disposable 127.0.0.1/postgres URL with explicit port")
    return raw


def verify_results(events, required=None, *, package=None, require_children=False, exact_roots=False, children_required=None):
    required = REQUIRED if required is None else required
    started, passed = set(), set()
    children = set()
    passed_children = set()
    package_passed = False
    count = 0
    for event in events:
        name = event.get("Test", "")
        root = name.split("/", 1)[0]
        action = event.get("Action")
        if action == "fail":
            raise RuntimeError("Go financial contract failed")
        if package is not None and name and event.get("Package") != package:
            raise RuntimeError("financial contract came from an unexpected package")
        if exact_roots and name and root not in required:
            raise RuntimeError("unexpected financial contract in scoped phase")
        if action == "run" and name and "/" not in name:
            started.add(name)
        if action == "pass":
            if name:
                count += 1
                if "/" not in name:
                    passed.add(name)
                else:
                    children.add(root)
                    passed_children.add(name)
            elif event.get("Package") == package:
                package_passed = True
        if action == "skip" and root in required:
            raise RuntimeError("required financial contract or subcase skipped")
    if required - passed or required - started:
        raise RuntimeError("required financial contracts did not execute: " + ", ".join(sorted(required - passed | required - started)))
    if require_children and (required if children_required is None else children_required) - children:
        raise RuntimeError("required financial contract has no completed child cases")
    for root, names in EXACT_REQUIRED_CHILDREN.items():
        if root in required:
            missing = {root + "/" + name for name in names} - passed_children
            if missing:
                raise RuntimeError("required financial contract subcases did not complete: " + ", ".join(sorted(missing)))
    if package is not None and not package_passed:
        raise RuntimeError("financial package did not complete successfully")
    return count


def main():
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go")

    def query(sql):
        result = subprocess.run(
            [psql, raw, "-X", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", sql],
            capture_output=True, text=True, encoding="utf-8", check=False,
        )
        if result.returncode:
            raise RuntimeError("disposable PostgreSQL operation failed")
        return result.stdout.strip()

    databases = query("SELECT datname FROM pg_database ORDER BY datname")
    user = query("SELECT current_user")
    roles = query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname")
    def assert_instance(expected_roles):
        if query("SELECT datname FROM pg_database ORDER BY datname") != databases or set(query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname").splitlines()) != expected_roles:
            raise RuntimeError("database or deployment actors differ; no fixture writes or cleanup permitted")
        if query("""SELECT
            (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema') +
            (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
             WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')""") != "0":
            raise RuntimeError("bootstrap database contains user objects; no writes permitted")
        if query("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()") != "0":
            raise RuntimeError("another database client is active; exclusive fixture required")

    if databases != "postgres\ntemplate0\ntemplate1" or roles != user:
        raise RuntimeError("database or deployment actors already exist; no writes permitted")
    assert_instance({user})
    if int(query("SHOW server_version_num")) < 160000:
        raise RuntimeError("PostgreSQL 16 or newer is required by existing actor grants")

    def run_phase(label, target, package, required, *, scoped=False):
        env = {key: value for key, value in os.environ.items() if not key.startswith("PINVOU_TEST_")}
        # Nested Go consumers inherit the same race/build contract as this phase.
        env["GOFLAGS"] = " ".join(filter(None, (env.get("GOFLAGS", ""), "-race -tags=nomsgpack -mod=readonly")))
        if target == "./internal/account":
            env["PINVOU_TEST_ACCOUNT_DATABASE_URL"] = raw
        elif target == "./internal/billingschema":
            env["PINVOU_TEST_SCHEMA_DATABASE_URL"] = raw
        else:
            env["PINVOU_TEST_BILLING_DATABASE_URL"] = raw
        if target == "./internal/token":
            env["PINVOU_TEST_TOKEN_DATABASE_URL"] = raw
        # The 59 retained Billing roots create about 250 isolated migration
        # fixtures. Other phases retain their existing finite 20-minute bound.
        timeout = "75m" if label == "retained-billing" else "20m"
        command = [go, "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=" + timeout, "-json"]
        if scoped:
            command += ["-run", "^(" + "|".join(sorted(required)) + ")$"] if len(required) > 1 else ["-run", "^" + next(iter(required)) + "$"]
        if label == "retained-token":
            # This root already ran with absent Billing actors above. Do not
            # rerun its exclusive fixture after bootstrapping those actors;
            # keep every other original Token package test selected.
            command += ["-skip", "^(" + "|".join(sorted(PROVISIONING_REQUIRED)) + ")$"]
        command.append(target)
        print(f"Financial phase: {label}", flush=True)
        result = subprocess.run(command, cwd=ROOT / "account-platform", env=env,
                                capture_output=True, text=True, encoding="utf-8", check=False)
        # Preserve real phase output even when Go failed, skipped or was truncated.
        sys.stdout.write(result.stdout)
        sys.stderr.write(result.stderr)
        if result.returncode:
            raise RuntimeError("Go financial phase failed: " + label)
        count = verify_results([json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")],
                               required, package=package, require_children=scoped or label == "retained-token", exact_roots=scoped,
                               children_required=TOKEN_CHILD_REQUIRED if label == "retained-token" else BILLING_CHILD_REQUIRED if label == "retained-billing" else None)
        print(f"Financial phase {label}: {count} named passes; all {len(required)} required roots completed", flush=True)

    # Each existing owner fixture requires its own absent actors. Restoration
    # is checked between every phase, before any next fixture can write.
    try:
        run_phase("owner-account", "./internal/account", ACCOUNT_PACKAGE, ACCOUNT_REQUIRED, scoped=True)
    finally:
        assert_instance({user})
    try:
        run_phase("owner-wallet", "./internal/billing", BILLING_PACKAGE, OWNER_REQUIRED, scoped=True)
    finally:
        assert_instance({user})
    try:
        run_phase("owner-schema", "./internal/billingschema", SCHEMA_PACKAGE, SCHEMA_REQUIRED, scoped=True)
    finally:
        assert_instance({user})
    try:
        run_phase("owner-provisioning", "./internal/token", TOKEN_PACKAGE, PROVISIONING_REQUIRED, scoped=True)
    finally:
        assert_instance({user})

    query("""
BEGIN;
CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT billing_service,billing_schema_owner TO billing_migrator;
GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE;
COMMIT;
""")
    try:
        run_phase("retained-token", "./internal/token", TOKEN_PACKAGE, REQUIRED)
        assert_instance(ACTORS | {user})
        run_phase("retained-billing", "./internal/billing", BILLING_PACKAGE, RETAINED_REQUIRED, scoped=True)
    finally:
        # Do not force-delete remnants from failed fixtures or unknown actors.
        assert_instance(ACTORS | {user})
        if query("""SELECT
            EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass
                   AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) AND dbid<>0)
            OR EXISTS(SELECT 1 FROM pg_auth_members
                      WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                             OR member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')))
                        AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))
                                AND member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))))""") != "f":
            raise RuntimeError("external actor dependencies; owned roles retained for diagnosis")
        query("BEGIN; DROP ROLE billing_migrator; DROP ROLE billing_schema_owner; DROP ROLE billing_service; COMMIT;")
        assert_instance({user})



if __name__ == "__main__":
    main()
