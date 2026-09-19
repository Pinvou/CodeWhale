package billing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// TestRefundExceptionRepairProofProjectionIsClosedAndActivationGated covers the
// GSA-30 proof-authority slice: the closed projection is recomputed by the owner
// domain, a rejected repair is a closed DENY reason, and no proof can be issued
// while the command's activation row is INACTIVE.
func TestRefundExceptionRepairProofProjectionIsClosedAndActivationGated(t *testing.T) {
	refundID, evidenceID, caseID := uuid.New(), uuid.New(), uuid.New()
	walletID, packageID := uuid.New(), uuid.New()
	updatedAt := time.Now().UTC().Truncate(time.Microsecond)
	frozen := approvalencoding.LegacyProofInput{
		CommandType: refundExceptionRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceID: refundID.String(),
		NormalizedCommand: marshalRaw(map[string]any{
			"command_type": refundExceptionRepairCommand, "policy_version": 1, "schema_version": 1,
			"refund_id": refundID.String(), "payment_evidence_id": evidenceID.String(),
			"anomaly_case_id": caseID.String(), "reason_code": refundExceptionRepairReason,
		}),
	}
	plan := RefundExceptionRepairPlan{
		RefundID: refundID, EvidenceID: evidenceID, AnomalyCaseID: caseID, AnomalyVersion: 4,
		AmountFen: 1200, CreditMicrocredits: 1200, RevokedMicrocredits: 900, ShortfallMicrocredits: 300,
		ProviderRefundID: "wx-refund-1", WalletID: walletID, WalletVersion: 7, PackageID: packageID,
		PackageStatus: "FROZEN", RefundUpdatedAt: updatedAt, FactsDigest: strings.Repeat("a", 64),
	}
	repository := &billingProofFactsStub{refundRepair: plan}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), repository, frozen)
	if err != nil || reason != "" {
		t.Fatalf("refund exception repair proof reason=%s err=%v", reason, err)
	}
	if ownerState != "wx-refund-1" {
		t.Fatalf("unexpected owner state %q", ownerState)
	}
	var critical, financial map[string]any
	if err := json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.FinancialSnapshot, &financial); err != nil {
		t.Fatal(err)
	}
	if critical["refund_updated_at"] != updatedAt.Format(time.RFC3339Nano) || critical["anomaly_version"] != float64(4) ||
		critical["facts_digest"] != plan.FactsDigest || critical["policy_version"] != float64(1) || len(critical) != 4 {
		t.Fatalf("unexpected critical projection %+v", critical)
	}
	if financial["amount_fen"] != float64(1200) || financial["credit_microcredits"] != float64(1200) ||
		financial["provider_refund_id"] != "wx-refund-1" || financial["revoked_microcredits"] != float64(900) ||
		financial["shortfall_microcredits"] != float64(300) || financial["wallet_id"] != walletID.String() ||
		financial["wallet_version"] != float64(7) || financial["package_id"] != packageID.String() || len(financial) != 8 {
		t.Fatalf("unexpected financial projection %+v", financial)
	}

	rejected := &billingProofFactsStub{refundRepairErr: ErrApprovalSnapshotMismatch}
	if _, reason, _, err := currentBillingProofInput(context.Background(), rejected, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("rejected repair reason=%s err=%v", reason, err)
	}
	mismatched := &billingProofFactsStub{refundRepair: plan}
	mismatched.refundRepair.RefundID = uuid.New()
	if _, reason, _, err := currentBillingProofInput(context.Background(), mismatched, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("mismatched repair reason=%s err=%v", reason, err)
	}
	wrongReason := approvalencoding.LegacyProofInput{
		CommandType: refundExceptionRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		NormalizedCommand: marshalRaw(map[string]any{
			"refund_id": refundID.String(), "payment_evidence_id": evidenceID.String(),
			"anomaly_case_id": caseID.String(), "reason_code": "SOMETHING_ELSE",
		}),
	}
	if _, _, _, err := currentBillingProofInput(context.Background(), repository, wrongReason); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("wrong reason code err=%v", err)
	}

	now := time.Now().UTC()
	reader := &billingApprovalProofStub{billingProofFactsStub: repository, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	if _, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{
		ApprovalRequestID: uuid.New(),
		Artifacts:         approvalencoding.ProofArtifacts{CommandType: refundExceptionRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1},
	}); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive refund exception repair proof err=%v", err)
	}
}

// The consumed authorization freezes only the wire-visible owner projection, so
// an empty facts digest means "the execution transaction recomputes it" while a
// presented digest must still match the owner revision exactly.
func TestRefundExceptionRepairFrozenFactsDigestIsOptionalOnlyWhenAbsent(t *testing.T) {
	plan := RefundExceptionRepairPlan{
		RefundID: uuid.New(), EvidenceID: uuid.New(), AnomalyCaseID: uuid.New(), AnomalyVersion: 2,
		AmountFen: 500, CreditMicrocredits: 500, RevokedMicrocredits: 500,
		ProviderRefundID: "wx-refund-2", WalletID: uuid.New(), WalletVersion: 3, PackageID: uuid.New(),
		RefundUpdatedAt: time.Now().UTC(), FactsDigest: strings.Repeat("b", 64),
	}
	frozen := RefundExceptionRepairFrozenFacts{
		RefundUpdatedAt: plan.RefundUpdatedAt, AnomalyVersion: plan.AnomalyVersion, AmountFen: plan.AmountFen,
		CreditMicrocredits: plan.CreditMicrocredits, ProviderRefundID: plan.ProviderRefundID,
		RevokedMicrocredits: plan.RevokedMicrocredits, ShortfallMicrocredits: plan.ShortfallMicrocredits,
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PackageID: plan.PackageID,
	}
	if mismatch := refundExceptionRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("owner-recomputed digest rejected: %s", mismatch)
	}
	frozen.FactsDigest = strings.Repeat("c", 64)
	if mismatch := refundExceptionRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("presented digest mismatch=%q", mismatch)
	}
	frozen.FactsDigest = plan.FactsDigest
	if mismatch := refundExceptionRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("matching digest rejected: %s", mismatch)
	}
	frozen.WalletVersion = plan.WalletVersion + 1
	if mismatch := refundExceptionRepairFrozenMismatch(plan, frozen); mismatch != "wallet_version" {
		t.Fatalf("wallet version mismatch=%q", mismatch)
	}
}
