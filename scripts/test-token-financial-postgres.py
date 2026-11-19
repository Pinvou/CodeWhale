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
from datetime import datetime
import json
import math
import os
import re
from pathlib import Path
import subprocess
import sys
import time
from urllib.parse import parse_qs, urlsplit

ROOT = Path(__file__).resolve().parents[1]
ACTORS = {"billing_migrator", "billing_schema_owner", "billing_service"}
REQUIRED = {
    "TestEmergencyCancellationDurableRetryPostgres",
    "TestStreamStatePollingPublicPostgres",
    "TestGovernanceUsageResourcePrechecksPostgres",
    "TestEmergencyCancellationWorkerDiagnosticsPostgres",
    "TestEmergencyCancellationClaimOwnershipPostgres",
    "TestEmergencyCancellationRequestLocksClaimPostgres",
    "TestUserUnusedIntentCancellationOwnerPostgres",
    "TestPendingUnusedExpiryRejectsInvalidProofTimestampPostgres",
    "TestPendingUnusedExpiryCancellationOwnerPostgres",
    "TestPendingUnusedExpiryLegacyCancellationFallbackPostgres",
    "TestPreparationFailurePreservesAcceptedUsagePostgres",
    "TestCircuitFailureBeforeDispatchPostgres",
    "TestDispatchCircuitReadOnlyRecoveryPublicPostgres",
    "TestDispatchCircuitOpenAfterAdmissionNeverBecomesProbePostgres",
    "TestDispatchCircuitRecoveryDoesNotRequireSharedHealthClaimPostgres",
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
    "TestReconciliationSequentialClaimsPublicPostgres",
    "TestF05DispatchLeaseLifecycle",
    "TestF05IncompleteUpstreamErrorReplayPublicPostgres",
    "TestF05MissingReferenceDeadlineFundsPublicPostgres",
    "TestF05CancellationKeyScopePublicPostgres",
    "TestF05CancellationRecoveryReconciliationOwnership",
    "TestF05CancellationRecoveryClosurePublicPostgres",
    "TestF05RotationRetainedContentBoundaries",
    "TestF05UnsettledContentRetention",
    "TestReplayRetentionBoundedCleanupPublicPostgres",
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
    "TestApprovalDeliveryTerminalRecoveryPostgres",
    "TestRestoredAdminReadsWithPublicMigrationsAndRuntimeActor",
    "TestApprovalDecisionOriginalReceiptPostgres",
    "TestApprovalDecisionLegacyConcurrentReceiptPostgres",
    "TestMemberUnitsReceiptSurvivesLegalUnitDeletion",
    "TestF06OrganizationStatusProjectionVersionPublicPostgres",
    "TestF06OrganizationDeletionVersionAndTerminalScope",
    "TestF06Review5566377593PublicPostgres",
    "TestOrganizationEvidenceTruncatePublicPostgres",
    "TestProviderQuotaConcurrentBoundary",
    "TestOrdinaryDeadLetterReplayRuntimePostgres",
    "TestIdentityRecoveryClaimsRuntimePostgres",
    "TestProfileSessionRevocationRecoveryRuntimePostgres",
    "TestProfileRevocationDurableAdmissionRuntimePostgres",
    "TestProfileRevocationClosureRuntimePostgres",
    "TestRegistrationCreateDispatchRuntimePostgres",
    "TestRegistrationUnknownInspectionPostgres",
    "TestRegistrationIdentityRejectionRuntimePostgres",
    "TestRegistrationCompensationDatabaseErrorIdentityPostgres",
    "TestRegistrationOutboxAggregateLookupPostgres",
    "TestPlatformRoleManagementRuntimePostgres",
    "TestUserCodeLookupRuntimePostgres",
    "TestAuditSearchOriginalMigrationRuntimePostgres",
    "TestDeliveredPlatformPermissionsPostgres",
    "TestTokenApprovalRecentAuthenticationPostgres",
    "TestTokenApprovalRecentAuthenticationLegacyWriterPostgres",
    "TestTokenAuthorizationWriterToBillingNonTargetPostgres",
    "TestOwnerCombinedActualHTTPPostgres",
    "TestApprovalRecoveryQueryAllRegisteredCommandsPostgres",
    "TestUnstartedApprovalGrantInventoryPostgres",
    "TestApprovalRecoveryQueryForwardMigrationPostgres",
    "TestApprovalAuthorizationWallClockPostgres",
    "TestApprovalMutationWallClockPostgres",
    "TestApprovalProofWallClockPostgres",
    "TestOrganizationUnitAuthorityWallClockPostgres",
    "TestApprovalListPoolRuntimePostgres",
    "TestApprovalPaginationSourcePostgres",
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
    "TestUnstartedApprovalGrantInventoryPostgres",
    "TestRegistrationBonusEligibilityCheckpointPostgresIntegration",
    "TestRegistrationBonusCheckpointGuardsPostgresIntegration",
    "TestRegistrationBonusCheckpointMigrationPostgresIntegration",
    "TestRegistrationBonusEligibilityReadyAndLifetimePostgresIntegration",
    "TestRegistrationBonusEligibilityBlockedConsumesNewMobilesPostgresIntegration",
}
ACCOUNT_CHILD_REQUIRED = ACCOUNT_REQUIRED - ACCOUNT_LEAF_REQUIRED
TOKEN_PACKAGE = "github.com/Pinvou/pinvou3/account-platform/internal/token"
TOKEN_CHILD_REQUIRED = {
    "TestEmergencyCancellationDurableRetryPostgres",
    "TestStreamStatePollingPublicPostgres",
    "TestGovernanceUsageResourcePrechecksPostgres",
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
SCHEMA_REQUIRED = {"TestBillingHistoricalCatalogRollbackPostgres", "TestBillingHistoricalStateRollbackPostgres", "TestBillingManualAdjustmentProofRollbackPostgres", "TestBillingOrdinaryReplayOriginalMigrationPostgres", "TestF04PublicTokenUserOwnerAuthorityAndDirectory", "TestF09EntitlementPackageIndexPublicMigration", "TestLatePaymentQueryRegisteredIdentity", "TestLegacyEntitlementUpgradeAdmissionPostgres", "TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres", "TestManualRetentionHistoricalHardeningPostgres", "TestOrphanPaidOrderIndexPublicMigration", "TestPaymentCreditClosureBoundaryPostgres", "TestPaymentCreditF12DeployedCarriersUpgradePostgres", "TestPaymentCreditRetentionPublicPostgres", "TestPublishedRollbackPackageIntegrityPostgres", "TestReconciliationFairRetryRegisteredIdentity", "TestTokenWalletReceiptSchemaPostgres", "TestUnknownProviderEntryPublicMigrationPostgres", "TestUsageWaiverHistoricalSchemaPostgres"}
F12_UPGRADE_PARENT = "TestPaymentCreditF12DeployedCarriersUpgradePostgres"
F12_UPGRADE_CHILD = "TestF12PaymentCreditUpgradeChildPostgres"
F12_UPGRADE_MARKER = "PINVOU_F12_UPGRADE_PHASE_RESULT "
# This parent verifies two exact child processes, not t.Run subcases. Its
# phase evidence is required separately below; every old subcase stays required.
SCHEMA_CHILD_REQUIRED = SCHEMA_REQUIRED - {F12_UPGRADE_PARENT}
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
    "TestF12PaymentRepairOrderLookupPostgres",
    "TestF12PaymentRepairDeployedBaselineUpgradePostgres",
    "TestRetainedOrganizationOrderHistoryPagination",
    "TestClosedGrantEvidenceCurrentRoleGuardsPostgres",
    "TestF04ExpiredHoldRecoveryPreservesUnknownAndReplaysRelease",
    "TestExpiredHoldProofRetryDiagnosticsPostgres",
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
    "TestPriceFallbackMissingBindingPostgres",
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
    "TestPaymentNotificationWorkerEscalatesUnmatchedTrustedCallback",
    "TestPaymentNotificationWorkerKeepsUnverifiedCallbacksOnRetry",
    "TestPaymentNotificationWorkerRollsBackAndRetriesManualCase",
    "TestDirectPaymentRepairCommandReceiptPostgres",
    "TestPaymentCreditJournalAdmissionGuardPostgres",
    "TestPaymentCreditResolverIntegrityPostgres",
    "TestEscalatedPaymentFinancialCompletionPostgres",
    "TestLatePaymentQueryCommandPostgres",
    "TestLatePaymentQueryClientHTTPPostgres",
    "TestLatePaymentArchiveCommandReplayPostgres",
    "TestProviderBlindCommandReceiptPostgres",
    "TestLatePaymentScheduleCommandReplayPostgres",
    "TestReconciliationFairRetryRuntimePostgres",
    "TestReconciliationUnknownDateOrphanLifecyclePostgres",
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
    "TestPackageExpiryCrossWalletIndexPostgres",
    "TestProviderHealthReadFailurePostgres",
    "TestProviderHealthGlobalWindowIndexPostgres",
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
    "TestF12PaymentRepairOrderLookupPostgres",
    "TestF12PaymentRepairDeployedBaselineUpgradePostgres",
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
    "TestPaymentNotificationWorkerEscalatesUnmatchedTrustedCallback",
    "TestPaymentNotificationWorkerKeepsUnverifiedCallbacksOnRetry",
    "TestPaymentNotificationWorkerRollsBackAndRetriesManualCase",
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
    "TestBillingManualAdjustmentProofRollbackPostgres": {
        f"{state}/{entry}" for state in ("empty", "retained-proof") for entry in ("public-api", "actual-cli", "renamed-history-cli")
    },
    "TestPaymentCreditJournalAdmissionGuardPostgres": {
        f"{entry}-{mode}"
        for entry in ("RECHARGE", "REPAIR_CREDIT")
        for mode in ("missing", "wrong-order", "wrong-payer", "already-applied")
    },
    "TestPaymentCreditResolverIntegrityPostgres": {
        "applied-without-effects", "prepared-with-effects",
        "missing-transaction", "fingerprint-mismatch",
    },
    "TestDirectPaymentRepairCommandReceiptPostgres": {
        "pre-admission-fixture-rejects-current-schema",
        "runtime-immutable-command-and-exact-prefix-down",
    },
    "TestStreamStatePollingPublicPostgres": {"complete", "cancel", "disconnect",
        "complete/concurrent-last-event-id-public-replay", "disconnect/concurrent-last-event-id-public-replay",
        "external-committed", "external-released"},
    "TestProfileRevocationDurableAdmissionRuntimePostgres": {
        "capacity-before-grant-confirmed-replay-and-unknown-retention",
        "cancel-before-authority-preserves-reservation",
        "dispatch-token-and-legacy-command-cannot-regrant",
        "concurrent-global-admission-across-profiles",
        "legacy-upgrade-preserves-confirmed-and-unknown-receipts",
        "capacity-public-complete-resumes-original-operation",
        "lease-expiry-during-global-admission-wait",
    },
    "TestProfileRevocationClosureRuntimePostgres": {
        "reserved-closure-seals-no-dispatch-and-releases-capacity",
        "confirmed-replays-after-anonymization",
        "late-confirmation-replays-after-anonymization",
    },
    "TestGovernanceUsageResourcePrechecksPostgres": {
        "all-active-states-preserve-full-facts", "repeated-read-stable-and-read-only",
        "historical-only-preserves-other-dependencies",
    },
    "TestEscalatedPaymentFinancialCompletionPostgres": {
        "lost-ack-replays-exact-committed-result",
        "concurrent-reviewers-complete-once",
        "actual-full-refund-receipt", "confirmation-audit-failure-rolls-back",
        "payment-repair-late", "payment-repair-duplicate",
        "no-effect-failed-evidence", "no-effect-legacy-order-only",
        "new-payment-evidence-keeps-blocked",
        "conflicting-original-amount", "conflicting-original-currency",
        "conflicting-original-customer", "conflicting-original-sku",
        "conflicting-original-channel",
    },
    "TestProviderQuotaConcurrentBoundary": {"migration-round-trip", "send", "verify", "login-registration", "login-profile"},
    "TestProviderHealthReadFailurePostgres": {"discovery", "latest-five", "failure-rate"},
    "TestApprovalPaginationSourcePostgres": {
        "registered-replicas-use-distinct-admitted-client-keys",
        "exhausted-client-is-shared-across-replicas-with-rollback",
        "subject-budget-survives-source-change",
    },
    "TestOrphanPaidOrderIndexPublicMigration": {
        "fresh-failure-atomicity-and-retry",
        "populated-upgrade-replay-and-own-ddl-rollback",
    },
    "TestApprovalDeliveryTerminalRecoveryPostgres": {
        "public-migration-rollback-and-upgrade",
        "ack-success-terminal-write-failure-worker-and-redelivery",
        "bounded-batches-replay-and-negative-states",
        "locked-row-and-concurrent-workers-then-next-pass",
        "permission-fault-zero-effect-and-restoration",
        "batch-sql-failure-is-atomic-and-next-call-recovers",
        "populated-rollback-retains-frozen-and-terminal-history",
    },
    "TestPackageExpiryCrossWalletIndexPostgres": {"fresh-install", "retained-upgrade"},
    "TestRegistrationOutboxAggregateLookupPostgres": {"fresh-install", "retained-upgrade"},
    "TestProviderHealthGlobalWindowIndexPostgres": {"fresh-install", "retained-upgrade"},
    "TestBillingHistoricalCatalogRollbackPostgres": {
        f"version{version}/seeded={seeded}{suffix}"
        for version in (35, 36, 37, 51)
        for seeded in ("false", "true")
        for suffix in ("", "/actual-cli")
    },
    "TestOrganizationStartupDiagnosticsPostgres": {
        "profile", "token", "billing-earlier-wallet-guard",
    },
    "TestPaymentCreditRetentionPublicPostgres": {"late-acknowledgement-survives-billing-retention", "waiting-old-ack-batch-does-not-starve-later-clearance"},
    "TestPaymentCreditClosureBoundaryPostgres": {
        "cleanup-wins-retains-evidence-without-credit",
        "cleanup-commit-during-admission-lock-wait",
        "inflight-credit-and-resolved-credit-invalidate-stale-ready",
        "admission-reply-loss-recovers-abort-and-manual-evidence",
        "rollback-after-grant-retains-abort-and-next-payment-recovers",
        "lost-ack-reply-resumes-metadata-without-repeating-money",
        "wrong-resolution-identity-retains-pending-then-valid-tick-recovers",
        "unconfigured-authority-retains-manual-payment-without-bypass",
        "pending-subject-dml-and-closed-admission-boundaries",
    },
    "TestPublishedRollbackPackageIntegrityPostgres": {
        "published-down-legacy-crlf-no-create", "recorded-up-identity-before-down", "unrelated-custom-history",
        "published-ledger-cannot-become-custom/LF", "published-ledger-cannot-become-custom/CRLF",
        "forward-only-history-cannot-authorize-rollback",
    },
    "TestUsageWaiverHistoricalSchemaPostgres": {
        "fresh-exact-history-append-only-runtime-and-forward-only",
        "old-LF/changed-current-down-before-up", "old-LF/changed-current-down-before-verify",
        "old-CRLF/changed-current-down-before-up", "old-CRLF/changed-current-down-before-verify",
        "unknown-checksum", "wrong-legacy-shape",
    },
    "TestF04ExpiredHoldRecoveryPreservesUnknownAndReplaysRelease": {
        "first-RPC-release-crash-keeps-durable-intent",
        "slow-proof-exhaustion-persists-backoff-and-next-healthy-progress",
    },
    "TestBillingHistoricalStateRollbackPostgres": {
        f"version{version}/seeded={seeded}{suffix}"
        for version in (10, 14, 27, 49)
        for seeded in ("false", "true")
        for suffix in ("", "/actual-cli")
    } | {
        f"version{version}/seeded=true/archived-cli" for version in (10, 14, 27, 49)
    } | {
        f"{scenario}{suffix}"
        for scenario in ("version14/repair-only", "version27/revoked-only", "version49/advanced-after-expiry")
        for suffix in ("", "/actual-cli")
    },
    "TestReplayRetentionBoundedCleanupPublicPostgres": {
        "one-large-unsettled-stream-is-row-bounded-and-retryable",
        "actual-worker-drains-full-batch-without-hourly-wait",
        "locked-oldest-is-skipped-and-next-cycle-resumes",
        "settled-stream-delete-is-bounded",
        "active-case-parent-lock-serializes-copy-with-cleanup",
        "tombstone-key-clear-is-atomic-bounded-and-protected-rows-do-not-starve",
        "case-delete-and-payload-cascade-are-bounded",
        "capacity-sample-10000-live-frames-plan-and-single-batch",
        "content-lock-timeout-rolls-back-and-next-worker-tick-recovers",
    },
    "TestExpiredHoldProofRetryDiagnosticsPostgres": {
        "quick-failure", "timeout", "cancel", "unavailable", "invalid-proof",
        "valid-proof", "persistence-failure", "operator",
    },
    "TestRechargeProductCommandAtomicityPostgres": {"active-provider-admission-candidate-progress"},
    "TestPriceFallbackMissingBindingPostgres": {
        "missing-binding-and-rollback",
        "permission-denied-before-mutation",
        "binding-read-fault-and-later-retry",
        "valid-binding-fallback-and-receipt-replay",
    },
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
    "TestEmergencyCancellationDurableRetryPostgres": {
        "failure-backoff-and-manual-case", "expired-claim-and-stale-result",
        "late-financial-review-is-independent", "settlement-does-not-close-emergency-case",
        "unstarted-tail-does-not-spend-budget",
    },
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
    "TestRetainedOrganizationOrderHistoryPagination": {
        "same-timestamp-cursor-and-owner-isolation",
        "concurrent-new-order-does-not-shift-older-pages",
    },
    "TestManagementPublicMigrationInstallation": {
        "fresh",
        "installed-29-upgrade",
        "installed-30-upgrade",
        "installed-f05-34-upgrade",
        "installed-f07-36-upgrade",
        "installed-governance-before-dispatch-40-upgrade",
        "installed-governance-before-dispatch-40-upgrade/dispatch-index-public-cli-round-trip",
        "installed-pre-cleanup-39-upgrade",
        "installed-pre-dispatch-index-40-upgrade",
        "installed-pre-dispatch-index-40-upgrade/dispatch-index-public-cli-round-trip",
        "installed-pre-emergency-retry-40-upgrade",
        "installed-pre-emergency-retry-40-upgrade/retry-forward-only-public-prefix",
        "installed-pre-governance-index-40-upgrade",
        "installed-pre-governance-index-40-upgrade/governance-index-public-cli-rollback",
        "installed-pre-publication-41-upgrade",
        "installed-pre-runtime-37-upgrade",
        "installed-published-42-upgrade",
        "installed-published-42-upgrade/retry-forward-only-public-prefix",
        "legacy-upgrade",
        "retained-management-lifecycle-upgrade",
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
    "TestReconciliationUnknownDateOrphanLifecyclePostgres": {
        "cross-date-concurrency", "legacy-and-ignore", "mixed-canonical-and-legacy", "observation-rollback",
        "stale-review-binding", "query-index-upgrade", "case-identity-recovery-fence", "legacy-unbound-recovery-fence",
    },
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


def f12_upgrade_phase(event):
    """Accept only the real schema parent's validated, secret-free summary."""
    if (event.get("Action") != "output" or event.get("Package") != SCHEMA_PACKAGE
            or event.get("Test") != F12_UPGRADE_PARENT):
        raise RuntimeError("F12 upgrade phase evidence has the wrong owner")

    def unique_fields(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate field")
            result[key] = value
        return result

    output = event["Output"]
    phases = []
    for line in output.splitlines():
        if F12_UPGRADE_MARKER not in line:
            continue
        try:
            value = json.loads(line.split(F12_UPGRADE_MARKER, 1)[1], object_pairs_hook=unique_fields)
        except (ValueError, TypeError):
            raise RuntimeError("invalid F12 upgrade phase evidence") from None
        if (not isinstance(value, dict)
                or set(value) != {"schema", "phase", "root", "package", "runs", "passes", "package_pass", "skips"}
                or type(value["schema"]) is not int or value["schema"] != 1
                or type(value["phase"]) is not str or value["phase"] not in {"historical", "current"}
                or value["root"] != F12_UPGRADE_CHILD or value["package"] != BILLING_PACKAGE
                or type(value["runs"]) is not int or value["runs"] != 1
                or type(value["passes"]) is not int or value["passes"] != 1
                or value["package_pass"] is not True
                or type(value["skips"]) is not int or value["skips"] != 0):
            raise RuntimeError("invalid F12 upgrade phase evidence")
        phases.append(value["phase"])
    return phases


def verify_results(events, required=None, *, package=None, require_children=False, exact_roots=False, children_required=None):
    required = REQUIRED if required is None else required
    started, passed = set(), set()
    children = set()
    passed_children = set()
    package_passed = False
    f12_phases = set()
    f12_root_actions = {"run": 0, "pass": 0}
    count = 0
    for event in events:
        name = event.get("Test", "")
        root = name.split("/", 1)[0]
        action = event.get("Action")
        if F12_UPGRADE_PARENT in required:
            if action == "skip" and event.get("Package") == SCHEMA_PACKAGE:
                raise RuntimeError("F12 upgrade schema package or contract skipped")
            if name == F12_UPGRADE_PARENT and action in f12_root_actions:
                f12_root_actions[action] += 1
            output = event.get("Output", "")
            if isinstance(output, str) and F12_UPGRADE_MARKER in output:
                for phase in f12_upgrade_phase(event):
                    if phase in f12_phases:
                        raise RuntimeError("duplicate F12 upgrade phase evidence")
                    f12_phases.add(phase)
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
    if F12_UPGRADE_PARENT in required and (
            package != SCHEMA_PACKAGE or f12_phases != {"historical", "current"}
            or f12_root_actions != {"run": 1, "pass": 1}):
        raise RuntimeError("F12 upgrade requires both exact child phases and parent completion")
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


INSTANCE_SNAPSHOT_SQL = """SELECT json_build_object(
    'databases', (SELECT array_agg(datname ORDER BY datname) FROM pg_database),
    'roles', (SELECT array_agg(rolname ORDER BY rolname) FROM pg_roles WHERE rolname !~ '^pg_'),
    'objects', (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
                WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema')
             + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
                WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'),
    'clients', (SELECT count(*) FROM pg_stat_activity
                WHERE backend_type='client backend' AND pid<>pg_backend_pid()))"""


def require_exclusive_instance(query, databases, roles, *, timeout_seconds=15, now=None, pause=None):
    """Observe a clean private cluster, without disconnecting another backend.

    A psql/Go process can exit before its PostgreSQL backend. Read all admission
    conditions on one identity-guarded connection per observation: separate
    catalog probes would themselves introduce more retiring client backends.
    Only a positive client count is eligible to wait; identity/query errors,
    catalog drift or malformed observations always fail immediately.
    """
    if type(timeout_seconds) not in (int, float) or not 0 < timeout_seconds <= 15:
        raise ValueError("PostgreSQL observation budget must be positive and at most 15 seconds")
    now = time.monotonic if now is None else now
    pause = time.sleep if pause is None else pause
    databases, roles = list(databases), set(roles)
    deadline = now() + timeout_seconds
    while True:
        remaining = deadline - now()
        if remaining <= 0:
            raise RuntimeError("another client is active; exclusive fixture required")
        try:
            snapshot = json.loads(query(INSTANCE_SNAPSHOT_SQL, timeout=remaining))
        except subprocess.TimeoutExpired:
            raise RuntimeError("PostgreSQL fixture observation timed out; no writes permitted") from None
        except (ValueError, TypeError):
            raise RuntimeError("invalid PostgreSQL fixture observation; no writes permitted") from None
        if (not isinstance(snapshot, dict) or set(snapshot) != {"databases", "roles", "objects", "clients"}
                or snapshot["databases"] != databases or not isinstance(snapshot["roles"], list)
                or any(not isinstance(role, str) for role in snapshot["roles"])
                or len(snapshot["roles"]) != len(set(snapshot["roles"]))
                or set(snapshot["roles"]) != roles):
            raise RuntimeError("database or actors differ; no fixture writes or cleanup permitted")
        if type(snapshot["objects"]) is not int or snapshot["objects"] != 0:
            raise RuntimeError("bootstrap database contains user objects; no writes permitted")
        clients = snapshot["clients"]
        if type(clients) is not int or clients < 0:
            raise RuntimeError("invalid PostgreSQL client observation; no writes permitted")
        if now() >= deadline:
            raise RuntimeError("another client is active; exclusive fixture required")
        if clients == 0:
            return
        pause(min(0.05, max(0, deadline - now())))


BILLING_GROUPS = tuple(f"billing-{index}" for index in range(4))
GROUPS = ("owners", "token", *BILLING_GROUPS)
OWNER_LANES = {
    "account": ("owner-account", "lifecycle-reconciliation"),
    "wallet": ("owner-wallet", "owner-token-schema", "owner-provisioning"),
    "schema": ("owner-schema-legacy-expiry", "owner-schema"),
}
OWNER_ORDER = ("owner-account", "owner-wallet", "owner-schema-legacy-expiry", "owner-schema", "owner-token-schema",
               "owner-provisioning", "lifecycle-reconciliation")
EXACT_REQUIRED_CHILDREN["TestOwnerWalletTemplateScopePostgres"] = {
    "first", "first/case-a", "first/case-b", "second", "second/case-a", "second/case-b",
}
EXACT_REQUIRED_CHILDREN["TestDatabaseTemplateIsolationPostgres"] = {
    "private-data-schema-and-frozen-source", "failed-preparation-never-becomes-hit",
    "disabled-path-keeps-empty-create",
}


def billing_partitions(roots, durations):
    """Assign complete roots by measured cost; unknown/new roots are never omitted."""
    fallback, weights = durations["default_seconds"], durations["seconds"]
    if (not isinstance(weights, dict) or not isinstance(fallback, (int, float))
            or isinstance(fallback, bool) or not math.isfinite(fallback) or fallback <= 0
            or any(not isinstance(value, (int, float)) or isinstance(value, bool)
                   or not math.isfinite(value) or value < 0 for value in weights.values())):
        raise ValueError("invalid financial test durations")
    buckets = {name: set() for name in BILLING_GROUPS}
    loads = {name: 0.0 for name in BILLING_GROUPS}
    for root in sorted(roots, key=lambda name: (-weights.get(name, fallback), name)):
        group = min(BILLING_GROUPS, key=lambda name: (loads[name], name))
        buckets[group].add(root)
        loads[group] += weights.get(root, fallback)
    return buckets


def billing_roots(group):
    # Keep each root's race/locking subcases and exclusive actor lifecycle intact.
    if group == "all":
        return RETAINED_REQUIRED
    if group not in BILLING_GROUPS:
        raise ValueError("not a Billing group")
    durations = json.loads((ROOT / "scripts/account-financial-test-durations.json").read_text(encoding="utf-8"))
    return billing_partitions(RETAINED_REQUIRED, durations)[group]


def run_go_phase(command, *, env):
    """Stream the original output while retaining JSON evidence for the gate."""
    output = []
    with subprocess.Popen(command, cwd=ROOT / "account-platform", env=env,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                          text=True, encoding="utf-8") as process:
        for line in process.stdout:
            sys.stdout.write(line)
            sys.stdout.flush()
            if line.startswith("{"):
                output.append(line)
        code = process.wait()
    return subprocess.CompletedProcess(command, code, "".join(output), "")


def phase_timing(label, seconds, events, code, *, started_epoch=None):
    roots = [event for event in events if event.get("Action") == "pass"
             and event.get("Test") and "/" not in event["Test"]]
    profile = {
        "schema": 1, "phase": label, "elapsed_seconds": seconds, "exit_code": code,
        "cache_enabled": os.environ.get("PINVOU_CI_FIXTURE_TEMPLATES") == "1",
        "root_passes": sorted(event["Test"] for event in roots),
        "slow_roots": [{"test": event["Test"], "seconds": event.get("Elapsed", 0)}
                       for event in sorted(roots, key=lambda event: event.get("Elapsed", 0), reverse=True)[:15]],
    }
    starts = [event for event in events if event.get("Action") == "start" and event.get("Time")]
    packages = [event for event in events if event.get("Action") == "pass" and not event.get("Test")]
    profile["package_elapsed_seconds"] = sum(event.get("Elapsed", 0) for event in packages)
    if starts and started_epoch is not None:
        # Go emits RFC3339Nano. Python 3.10 accepts only microsecond precision;
        # truncate the diagnostic timestamp without changing test deadlines.
        timestamp = re.sub(r"(\.\d{6})\d+", r"\1", starts[0]["Time"]).replace("Z", "+00:00")
        profile["pre_test_seconds"] = max(0, datetime.fromisoformat(timestamp).timestamp() - started_epoch)
    operations = {}
    snapshots = {"prepared": 0, "reused": 0, "remaining": 0}
    for event in events:
        for line in event.get("Output", "").splitlines():
            if line.startswith("[ci-fixture-timing] "):
                timing = json.loads(line[len("[ci-fixture-timing] "):])
                value = operations.setdefault(timing["operation"], {"count": 0, "seconds": 0})
                value["count"] += 1
                value["seconds"] += timing["seconds"]
            if line.startswith("[ci-fixture-template] "):
                counts = json.loads(line[len("[ci-fixture-template] "):])
                for name in snapshots:
                    snapshots[name] += counts[name]
    profile["fixture_operations"] = operations
    profile["templates"] = snapshots
    profile["timing_note"] = "template-clone includes database-create; template-prepare includes migration-initialize; operation totals overlap"
    print("Financial timing: " + json.dumps(profile, ensure_ascii=False), flush=True)
    directory = os.environ.get("PINVOU_CI_RESOURCE_LEDGER")
    if directory:
        # Every .json in the ledger is an ownership lease. Diagnostics must
        # not masquerade as leases or weaken the existing retirement guard.
        target = Path(directory) / ("financial-phase-" + label + ".txt")
        target.write_text(json.dumps(profile, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


def schema_batches():
    # Keep the slow historical-upgrade root intact. New required roots join the
    # second batch; neither batch may be empty or introduce an unknown root.
    historical = {"TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres"}
    if not historical < SCHEMA_REQUIRED:
        raise RuntimeError("owner schema requires two nonempty complete-root batches")
    return (("owner-schema-legacy-expiry", historical),
            ("owner-schema", SCHEMA_REQUIRED - historical))


def main(*, group="all", verify_fixture_cache=False, owner_lane=None):
    if group not in ("all", *GROUPS):
        raise ValueError("unknown financial execution group")
    if owner_lane is not None and (group != "owners" or owner_lane not in OWNER_LANES):
        raise ValueError("owner lane requires the owners group")
    raw = local_database_url(os.environ.get("PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL", ""))
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    go = os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go")

    def query(sql, *, timeout=None):
        result = subprocess.run(
            [psql, raw, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", *ci_identity_guard(raw), "-c", sql],
            capture_output=True, text=True, encoding="utf-8", check=False, timeout=timeout,
        )
        if result.returncode:
            raise RuntimeError("disposable PostgreSQL operation failed")
        return result.stdout.strip()

    databases = query("SELECT datname FROM pg_database ORDER BY datname")
    user = query("SELECT current_user")
    roles = query("SELECT rolname FROM pg_roles WHERE rolname !~ '^pg_' ORDER BY rolname")
    def assert_instance(expected_roles):
        require_exclusive_instance(query, databases.splitlines(), expected_roles)

    if databases != "postgres\ntemplate0\ntemplate1" or roles != user:
        raise RuntimeError("database or deployment actors already exist; no writes permitted")
    assert_instance({user})
    if int(query("SHOW server_version_num")) < 160000:
        raise RuntimeError("PostgreSQL 16 or newer is required by existing actor grants")

    def run_phase(label, target, package, required, *, scoped=False):
        env = {key: value for key, value in os.environ.items() if not key.startswith("PINVOU_TEST_")}
        # Only these phases own stable outer Billing actors. In owners phases,
        # absent-actor fixtures must retire their local snapshots before roles.
        env["PINVOU_CI_RETAINED_BILLING_TEMPLATES"] = "1" if label in {"retained-billing", "retained-token"} else "0"
        env["PINVOU_CI_OWNER_WALLET_TEMPLATES"] = "1" if label in {"owner-wallet", "owner-wallet-scope-isolation"} else "0"
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
        # fixtures. Retained Token keeps its full package within a 40-minute
        # test-binary budget; other phases retain their 20-minute bound.
        timeout = {"retained-billing": "75m", "retained-token": "40m"}.get(label, "20m")
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
        started = time.monotonic()
        started_epoch = time.time()
        result = run_go_phase(command, env=env)
        events = [json.loads(line) for line in result.stdout.splitlines() if line.startswith("{")]
        phase_timing(label, time.monotonic() - started, events, result.returncode, started_epoch=started_epoch)
        if result.returncode:
            raise RuntimeError("Go financial phase failed: " + label)
        count = verify_results(events,
                               required, package=package, require_children=scoped or label == "retained-token", exact_roots=scoped,
                               children_required=SCHEMA_CHILD_REQUIRED & required if label == "owner-schema" else ACCOUNT_CHILD_REQUIRED if label == "owner-account" else TOKEN_CHILD_REQUIRED if label == "retained-token" else BILLING_CHILD_REQUIRED & required if label == "retained-billing" else None)
        print(f"Financial phase {label}: {count} named passes; all {len(required)} required roots completed", flush=True)

    if verify_fixture_cache:
        run_phase("fixture-template-isolation", "./internal/cifixture",
                  "github.com/Pinvou/pinvou3/account-platform/internal/cifixture",
                  {"TestDatabaseTemplateIsolationPostgres"}, scoped=True)
        assert_instance({user})

    if group in ("all", "owners"):
        # Each existing owner fixture requires its own absent actors. Restoration
        # is checked between every phase, before any next fixture can write.
        phases = {
            "owner-account": ("./internal/account", ACCOUNT_PACKAGE, ACCOUNT_REQUIRED),
            "owner-wallet": ("./internal/billing", BILLING_PACKAGE, OWNER_REQUIRED),
            "owner-token-schema": ("./internal/tokenschema", TOKEN_SCHEMA_PACKAGE, TOKEN_SCHEMA_REQUIRED),
            "owner-provisioning": ("./internal/token", TOKEN_PACKAGE, PROVISIONING_REQUIRED),
            "lifecycle-reconciliation": ("./internal/account", ACCOUNT_PACKAGE, LIFECYCLE_RECONCILIATION_REQUIRED),
        }
        # Preserve main's complete-root schema batches inside the isolated schema lane.
        # Each batch keeps its own 20m bound and mandatory pristine-instance check.
        phases.update({label: ("./internal/billingschema", SCHEMA_PACKAGE, required)
                       for label, required in schema_batches()})
        if owner_lane == "wallet" and verify_fixture_cache:
            try:
                run_phase("owner-wallet-scope-isolation", "./internal/billing", BILLING_PACKAGE,
                          {"TestOwnerWalletTemplateScopePostgres"}, scoped=True)
            finally:
                assert_instance({user})
        for label in OWNER_LANES[owner_lane] if owner_lane else OWNER_ORDER:
            try:
                run_phase(label, *phases[label], scoped=True)
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
    parser.add_argument("--verify-fixture-cache", action="store_true")
    parser.add_argument("--owner-lane", choices=tuple(OWNER_LANES))
    args = parser.parse_args()
    if args.owner_lane:
        # Affinity is inherited by Go, Node and CLI grandchildren. It limits
        # only this worker tree, never the Runner or another job's processes.
        cpus = json.loads(os.environ["PINVOU_CI_OWNER_CPU_SET"])
        if (not isinstance(cpus, list) or not 1 <= len(cpus) <= 2
                or any(type(cpu) is not int for cpu in cpus)
                or len(set(cpus)) != len(cpus) or not set(cpus) <= os.sched_getaffinity(0)):
            raise ValueError("invalid owner worker CPU affinity")
        os.sched_setaffinity(0, cpus)
        if os.sched_getaffinity(0) != set(cpus):
            raise RuntimeError("owner worker CPU affinity was not applied")
    if args.group == "owners" and not args.owner_lane and os.environ.get("PINVOU_CI_OWNER_PARALLEL") == "1":
        import runpy
        runpy.run_path(str(ROOT / "scripts/test-financial-owners.py"))["main"]()
    else:
        main(group=args.group, verify_fixture_cache=args.verify_fixture_cache, owner_lane=args.owner_lane)
