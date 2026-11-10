#!/usr/bin/env python3
"""Run existing Token and scoped organization financial contracts on disposable PG.

The Account fixture first owns its eight absent actors, then wallet
evidence and provisioning fixtures own their absent actors exclusively.
After strict restoration, retained fixtures create their own UUID databases and
Token actors while this runner owns the three Billing deployment actors. Only
the three Billing deployment actors are bootstrapped here, using their existing
privilege contract. This runner must never accept a deployed database URL.
"""
import argparse
import json
import os
from pathlib import Path
import subprocess
import sys
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[1]
ACTORS = {"billing_migrator", "billing_schema_owner", "billing_service"}
REQUIRED = {
    "TestEmergencyCancellationWorkerDiagnosticsPostgres",
    "TestEmergencyCancellationClaimOwnershipPostgres",
    "TestEmergencyCancellationRequestLocksClaimPostgres",
    "TestUserUnusedIntentCancellationOwnerPostgres",
    "TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres",
    "TestPendingUnusedExpiryCancellationOwnerPostgres",
    "TestPendingUnusedExpiryLegacyCancellationFallbackPostgres",
    "TestPreparationFailurePreservesAcceptedUsagePostgres",
    "TestCircuitFailureBeforeDispatchPostgres",
    "TestCredentialRotationWindowReceiptPostgres",
    "TestCredentialRotationOtherActorEffectPostgres",
    "TestCredentialFailurePersistenceBoundariesPostgres",
    "TestCredentialFailurePreparationFencePostgres",
    "TestCredentialFailureExistingEventsPostgres",
    "TestCredentialWindowRevocationOwnerPostgres",
    "TestHoldAcknowledgementSurvivesDispatchPersistenceFailurePostgres",
    "TestCancellationReceivedReadRechecksReservationPostgres",
    "TestCancellationFirstUserPreservesConfirmedReceiptPostgres",
    "TestModelTerminalObservationPreservesExpiryProofPostgres",
    "TestModelTerminalObservationRejectsUntrustedEvidencePostgres",
    "TestPersonalZeroBudgetCancellationAndExpiryPostgres",
    "TestUsageWaiverCoordinatorRecoveryPostgres",
    "TestF05AdditionalReservationProjectionPublicPostgres",
    "TestF06OrganizationAdmissionBoundariesPublicPostgres",
    "TestOrganizationModelPolicyPublicPostgres",
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
    "TestLifecycleForceCancelApprovalPostgres",
    "TestManualReviewSLAEscalationPostgres",
    "TestLocalModelCancellationHandshakeHTTP",
    "TestRemoteModelCancellationOwnerLoopTransport",
    "TestManagementCancellationHoldRecoveryRequiresRecordedProof",
}

ACCOUNT_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/account"
LIFECYCLE_RECONCILIATION_REQUIRED = {"TestLifecycleV3ApprovalReconciliationPostgres"}
ACCOUNT_REQUIRED = {
    "TestOrganizationStartupDiagnosticsPostgres",
    "TestRestoredAdminReadsWithPublicMigrationsAndRuntimeActor",
    "TestApprovalDecisionOriginalReceiptPostgres",
    "TestApprovalDecisionLegacyConcurrentReceiptPostgres",
    "TestMemberUnitsReceiptSurvivesLegalUnitDeletion",
    "TestF06OrganizationStatusProjectionVersionPublicPostgres",
    "TestF06OrganizationDeletionVersionAndTerminalScope",
    "TestF06Review5566377593PublicPostgres",
    "TestOrganizationEvidenceTruncatePublicPostgres",
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
    "TestRegistrationBonusEligibilityCheckpointPostgresIntegration",
    "TestRegistrationBonusCheckpointGuardsPostgresIntegration",
    "TestRegistrationBonusCheckpointMigrationPostgresIntegration",
    "TestRegistrationBonusCheckpointForeignFactsPostgresIntegration",
    "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration",
    "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration",
}
# These Account roots are real named leaf tests; all other required roots
# still need completed child-case evidence. Root run/pass and no-skip stay mandatory.
ACCOUNT_LEAF_REQUIRED = {
    "TestRegistrationBonusEligibilityCheckpointPostgresIntegration",
    "TestRegistrationBonusCheckpointGuardsPostgresIntegration",
    "TestRegistrationBonusCheckpointMigrationPostgresIntegration",
    "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration",
    "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration",
}
ACCOUNT_CHILD_REQUIRED = ACCOUNT_REQUIRED - ACCOUNT_LEAF_REQUIRED
TOKEN_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/token"
TOKEN_CHILD_REQUIRED = {
    "TestOrganizationModelPolicyPublicPostgres",
    "TestEmergencyCancellationClaimOwnershipPostgres",
    "TestEmergencyCancellationRequestLocksClaimPostgres",
    "TestUserUnusedIntentCancellationOwnerPostgres",
    "TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres",
    "TestPendingUnusedExpiryCancellationOwnerPostgres",
    "TestPendingUnusedExpiryLegacyCancellationFallbackPostgres",
    "TestCancellationReceivedReadRechecksReservationPostgres",
    "TestCancellationFirstUserPreservesConfirmedReceiptPostgres",
    "TestModelTerminalObservationPreservesExpiryProofPostgres",
    "TestModelTerminalObservationRejectsUntrustedEvidencePostgres",
    "TestLifecycleForceCancelApprovalPostgres",
    "TestLocalModelCancellationHandshakeHTTP",
    "TestRemoteModelCancellationOwnerLoopTransport",
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
TOKEN_SCHEMA_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
TOKEN_SCHEMA_REQUIRED = {"TestManagementPublicMigrationInstallation"}
SCHEMA_REQUIRED = {"TestLatePaymentQueryRegisteredIdentity", "TestReconciliationFairRetryRegisteredIdentity", "TestF09EntitlementPackageIndexPublicMigration", "TestTokenWalletReceiptSchemaPostgres", "TestLegacyEntitlementUpgradeAdmissionPostgres", "TestUsageWaiverHistoricalSchemaPostgres", "TestBillingOrdinaryReplayOriginalMigrationPostgres", "TestUnknownProviderEntryPublicMigrationPostgres", "TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres", "TestF04PublicTokenUserOwnerAuthorityAndDirectory", "TestManualRetentionHistoricalHardeningPostgres", "TestBillingHistoricalCatalogRollbackPostgres"}
OWNER_REQUIRED = {
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
    "TestClosedGrantEvidenceCurrentRoleGuardsPostgres",
    "TestEscalatedForcedRefundRequiresSecondActorAndReplays",
    "TestEscalatedForcedRefundWalletLockOrderPostgres",
    "TestEscalatedForcedRefundAtomicRollbackPostgres",
    "TestEscalatedForcedRefundEvidenceAndReceiptGuardsPostgres",
    "TestOrdinaryForcedRefundAppliedReceiptRecoveryPostgres",
    "TestReconciliationOrphanPaidOrderPostgres",
    "TestPaymentPersonalWalletAdmissionPostgres",
    "TestPaymentPersonalWalletAdmissionStatusRacePostgres",
    "TestReceivableSettlementGuardedWritesPostgres",
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
    "TestProductManualAdmissionPostgres",
    "TestExpirePendingManualProductAdmissionsReleasesFingerprintSlot",
    "TestProductAdmissionMaterialStorePostgres",
    "TestProductAdmissionMaterialPurgePostgres",
    "TestProductAdmissionMaterialHTTPPostgres",
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
    "TestExpirePendingManualProductAdmissionsReleasesFingerprintSlot",
    "TestProductAdmissionMaterialStorePostgres",
    "TestProductAdmissionMaterialPurgePostgres",
    "TestProductAdmissionMaterialHTTPPostgres",
    "TestEscalatedForcedRefundRequiresSecondActorAndReplays",
    "TestEscalatedForcedRefundWalletLockOrderPostgres",
    "TestOrdinaryForcedRefundAppliedReceiptRecoveryPostgres",
    "TestPaymentPersonalWalletAdmissionStatusRacePostgres",
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
    "TestBillingHistoricalCatalogRollbackPostgres": {
        f"version{version}/seeded={seeded}{suffix}"
        for version in (35, 36, 37, 51)
        for seeded in ("false", "true")
        for suffix in ("", "/actual-cli")
    },
    "TestOrganizationStartupDiagnosticsPostgres": {
        "profile", "token", "billing-earlier-wallet-guard",
    },
    "TestRechargeProductCommandAtomicityPostgres": {"active-provider-admission-candidate-progress"},
    "TestPostgresManagedPricePagination": {
        "bounded-batch-queries",
        *{f"bounded-batch-queries/{kind}-{size}" for kind in ("list", "namespace") for size in (1, 100, 200)},
        "bounded-batch-queries/empty-page",
        "bounded-batch-queries/model-zero-and-inherited-units",
        "bounded-batch-queries/absent-namespace",
        "bounded-batch-queries/snapshot-and-next-read",
        "bounded-batch-queries/database-faults-and-recovery",
        *{f"bounded-batch-queries/database-faults-and-recovery/{table}" for table in ("pricing_versions", "pricing_version_unit_configs", "pricing_space_versions")},
        "bounded-batch-queries/cancelled-and-next-read",
        "bounded-batch-queries/abnormal-read-shapes",
        "bounded-batch-queries/abnormal-read-shapes/empty-units",
        "bounded-batch-queries/abnormal-read-shapes/missing-namespace",
    },
    "TestProductManualAdmissionPostgres": {
        "two-explicit-humans-first-publish-and-immutable-replay",
        "runtime-cannot-rewrite-frozen-manual-facts",
        "concurrent-second-votes-issue-once-and-audit-failure-rolls-back",
        "reject-retains-original-and-no-evidence",
        "stale-product-and-foreign-original-rejected",
        "retention-purges-submission-snapshot-and-review-reason",
    },
    "TestEmergencyCancellationClaimOwnershipPostgres": {"before-request", "after-request", "pending", "zero-attempt"},
    "TestReceivableSettlementGuardedWritesPostgres": {"recharge-and-write-off-lock-order"},
    "TestEmergencyCancellationRequestLocksClaimPostgres": {"usage-lock-reclaim"},
    "TestPasswordPreparationAuthorizationPostgres": {
        "retention-redacts-expired-mobiles-without-changing-grant-facts/expired",
        "retention-redacts-expired-mobiles-without-changing-grant-facts/young",
        "retention-redacts-expired-mobiles-without-changing-grant-facts/valid",
        "retention-keeps-issued-grant-and-continues-later-artifact-cleanup",
        "retention-skips-locked-expired-captcha-and-rejects-late-grant",
        "retention-FK-protects-inflight-reference-and-subsequent-passes",
    },
    "TestOrganizationEvidenceTruncatePublicPostgres": {
        scenario + suffix
        for scenario in ("fresh-install", "retained-upgrade")
        for suffix in ("", "/organization_creation_requests", "/organization_unit_commands",
                       "/membership_commands", "/legal-replay-after-rejection", "/forward-only-public-down")
    },
    "TestClosedGrantEvidenceCurrentRoleGuardsPostgres": {
        "account_closure_grant_isolations", "account_closed_grant_reclaim_commands",
    },
    "TestManagementPublicMigrationInstallation": {
        "fresh", "legacy-upgrade", "installed-29-upgrade", "installed-30-upgrade",
        "installed-f05-34-upgrade", "retained-management-lifecycle-upgrade",
        "installed-f07-36-upgrade", "installed-pre-runtime-37-upgrade",
        "installed-pre-cleanup-39-upgrade",
    },
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
        "model-origin",
        "user-local-pre-dispatch",
    },
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
    "TestCancellationFirstUserPreservesConfirmedReceiptPostgres": {
        "model-before-attempt", "expiry-proof-ack-replay",
    },
    "TestCancellationReceivedReadRechecksReservationPostgres": {
        "not-started", "reserve-ack-pending", "reserve-ack-attached", "wrong-hold-then-original",
    },
    "TestRegistrationBonusCheckpointForeignFactsPostgresIntegration": {
        "unbound-existing-facts", "foreign-key-before-finalize",
    },
    "TestEscalatedForcedRefundAtomicRollbackPostgres": {
        "terminal-statement", "receipt-statement", "deferred-commit",
    },
    "TestEscalatedForcedRefundEvidenceAndReceiptGuardsPostgres": {
        "manual-status", "manual-link", "order-link", "transaction-link",
        "evidence-link", "receipt-identity-and-terminal-replay",
    },
    "TestOrganizationModelPolicyPublicPostgres": {
        "initial-default-is-not-a-stored-policy",
        "invalid-input-preserves-original-policy-and-next-write",
        "nil-and-explicit-empty-model-sets-have-identical-meaning",
        "immutable-replay-after-later-revision-and-actor-or-facts-conflict",
        "same-key-first-writers-replay-one-version",
        "different-key-initial-CAS-has-one-winner",
        "stale-CAS-and-unavailable-model-do-not-write",
    },
    "TestReconciliationOrphanPaidOrderPostgres": {"fresh-install", "historical-upgrade"},
    "TestRemoteModelCancellationOwnerLoopTransport": {
        "remote_emergency", "remote_emergency_then_user", "user_only",
        "read_failure", "lease_failure", "natural_terminal",
        "initial_requested", "initial_read_failure", "initial_lease_failure",
    },
    "TestLocalModelCancellationHandshakeHTTP": {
        "before_registration", "during_persistent_read", "after_persistent_read",
        "read_failure_preserves_funds", "normal", "user_stop_preserves_generation",
        "emergency_then_user_before_registration",
    },
    "TestLifecycleV3ApprovalReconciliationPostgres": {
        "wrong_durable_fail_closed",
        "safe_v3_false_true_and_v2_dry_apply_next_tick",
        "expired_approved_v3_missing_history",
        "expired_execution_failed_v3_missing_history",
        "unknown_version_fail_closed",
    },
    "TestLifecycleForceCancelApprovalPostgres": {
        "v2-false-compatibility", "v3-false", "v3-true-removed",
        "v3-true-removed/emergency-then-user-origin-retains-authority",
        "v3-true-removed/emergency-then-user-origin-retains-authority/owner-heartbeat-observes-remote-request",
    },
    "TestManualReviewSLAEscalationPostgres": {
        "two_workers_and_strict_one_hour_boundary",
        "database_clock_controls_deadlines_and_cooldown",
        "later_candidate_starts_a_fresh_cooldown",
        "closed_before_write",
        "deadlines_completed_before_write",
        "current_owner_and_stage_after_lock",
        "outbox_failure_rolls_back_and_retries",
    },
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


def ci_identity_guard(raw):
    # psql executes this guard and the requested SQL on the SAME backend.
    port = os.environ.get("PINVOU_ACCOUNT_CI_PG_PORT", "")
    if not any(os.environ.get(key) for key in ("PINVOU_ACCOUNT_CI_PG_PORT", "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID", "PINVOU_ACCOUNT_CI_PG_DIRECTORY")) and os.environ.get("GITHUB_ACTIONS") != "true":
        return []
    expected_url = "postgres://postgres@127.0.0.1:" + port + "/postgres?sslmode=disable"
    if os.environ.get("GITHUB_ACTIONS") != "true" or not port.isascii() or not port.isdecimal() or str(int(port)) != port or not 1024 <= int(port) <= 65535 or raw != expected_url:
        raise RuntimeError("CI service endpoint metadata required")
    system = os.environ.get("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID", "")
    directory = os.environ.get("PINVOU_ACCOUNT_CI_PG_DIRECTORY", "")
    if not system.isascii() or not system.isdecimal() or int(system) == 0 or directory not in ("/var/lib/postgresql/data", "/var/lib/postgresql/18/docker"):
        raise RuntimeError("CI service identity metadata required")
    guard = """DO $ci$ BEGIN
    IF NOT (session_user='postgres' AND current_user='postgres' AND current_database()='postgres'
      AND current_setting('port')='5432' AND current_setting('data_directory')='%s'
      AND (SELECT rolsuper FROM pg_roles WHERE rolname=session_user)
      AND (SELECT system_identifier::text FROM pg_control_system())='%s') THEN
      RAISE EXCEPTION 'CI service identity changed; no writes or cleanup permitted';
    END IF; END $ci$;""" % (directory, system)
    return ["-c", guard]


GROUPS = ("owners", "token", "billing-0", "billing-1")


def billing_roots(group):
    # Partition complete roots, never individual race/locking subcases. Sorting
    # makes both shards deterministic; their union always includes new roots.
    if group == "all":
        return RETAINED_REQUIRED
    if group not in ("billing-0", "billing-1"):
        raise ValueError("not a Billing group")
    return set(sorted(RETAINED_REQUIRED)[int(group[-1])::2])


def main(*, group="all"):
    if group not in ("all", *GROUPS):
        raise ValueError("unknown financial execution group")
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go")

    def query(sql):
        result = subprocess.run(
            [psql, raw, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", *ci_identity_guard(raw), "-c", sql],
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
        elif target == "./internal/tokenschema":
            env["PINVOU_TEST_TOKEN_DATABASE_URL"] = raw
        else:
            env["PINVOU_TEST_BILLING_DATABASE_URL"] = raw
        if target == "./internal/token":
            env["PINVOU_TEST_TOKEN_DATABASE_URL"] = raw
        if label == "retained-token":
            env["PINVOU_TEST_ACCOUNT_DATABASE_URL"] = raw
        if label == "lifecycle-reconciliation":
            # A separate exact Account root owns/restores its absent actors.
            # Never run the whole Account package in the Token/Billing phase.
            env["PINVOU_TEST_TOKEN_DATABASE_URL"] = raw
            env["PINVOU_TEST_BILLING_DATABASE_URL"] = raw
        # The retained Billing roots create hundreds of isolated migration
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
                               children_required=ACCOUNT_CHILD_REQUIRED if label == "owner-account" else TOKEN_CHILD_REQUIRED if label == "retained-token" else BILLING_CHILD_REQUIRED & required if label == "retained-billing" else None)
        print(f"Financial phase {label}: {count} named passes; all {len(required)} required roots completed", flush=True)

    if group in ("all", "owners"):
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
            run_phase("owner-token-schema", "./internal/tokenschema", TOKEN_SCHEMA_PACKAGE, TOKEN_SCHEMA_REQUIRED, scoped=True)
        finally:
            assert_instance({user})
        try:
            run_phase("owner-provisioning", "./internal/token", TOKEN_PACKAGE, PROVISIONING_REQUIRED, scoped=True)
        finally:
            assert_instance({user})

        try:
            run_phase("lifecycle-reconciliation", "./internal/account", ACCOUNT_PACKAGE,
                      LIFECYCLE_RECONCILIATION_REQUIRED, scoped=True)
        finally:
            assert_instance({user})

        if group == "owners":
            return

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
        if group in ("all", "token"):
            run_phase("retained-token", "./internal/token", TOKEN_PACKAGE, REQUIRED)
            assert_instance(ACTORS | {user})
        if group == "all" or group.startswith("billing-"):
            run_phase("retained-billing", "./internal/billing", BILLING_PACKAGE, billing_roots(group), scoped=True)
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
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--group", choices=("all", *GROUPS), default="all")
    main(group=parser.parse_args().group)
