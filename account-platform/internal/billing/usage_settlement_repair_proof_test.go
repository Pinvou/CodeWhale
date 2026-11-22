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

// TestUsageSettlementRepairProofProjectionIsClosedAndActivationGated covers the
// GSA-36 proof-authority slice: the closed projection carries the owner-derived
// expected/settled/overage split and both digests, a rejected repair is a closed
// DENY reason, and no proof can be issued while the command's activation row is
// INACTIVE.
func TestUsageSettlementRepairProofProjectionIsClosedAndActivationGated(t *testing.T) {
	holdID, usageRequestID, lossReviewID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	holdUpdatedAt, reviewUpdatedAt := time.Now().UTC().Truncate(time.Microsecond), time.Now().UTC().Truncate(time.Microsecond)
	frozen := approvalencoding.LegacyProofInput{
		CommandType: usageSettlementRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "USAGE_HOLD", TargetResourceID: holdID.String(),
		NormalizedCommand: marshalRaw(map[string]any{
			"usage_request_id": usageRequestID.String(), "hold_id": holdID.String(),
			"evidence_kind": usageSettlementRepairEvidenceKind, "loss_review_id": lossReviewID.String(),
			"reason_code": usageSettlementRepairReason,
		}),
	}
	plan := UsageSettlementRepairPlan{
		UsageRequestID: usageRequestID, HoldID: holdID, LossReviewID: lossReviewID, WalletID: walletID,
		WalletVersion: 4, HoldUpdatedAt: holdUpdatedAt, LossReviewUpdatedAt: reviewUpdatedAt,
		EvidenceKind: usageSettlementRepairEvidenceKind, PolicyVersion: 1, ExpectedMicrocredits: 150,
		SettledMicrocredits: 100, OverageMicrocredits: 50,
		AllocationsDigest: strings.Repeat("a", 64), FactsDigest: strings.Repeat("b", 64),
	}
	repository := &usageRepairProofFactsStub{usageRepair: plan}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), repository, frozen)
	if err != nil || reason != "" {
		t.Fatalf("usage repair proof reason=%s err=%v", reason, err)
	}
	if ownerState != usageRequestID.String() {
		t.Fatalf("unexpected owner state %q", ownerState)
	}
	var critical, financial map[string]any
	if err := json.Unmarshal(current.CriticalResourceVersions, &critical); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(current.FinancialSnapshot, &financial); err != nil {
		t.Fatal(err)
	}
	if critical["hold_updated_at"] != holdUpdatedAt.Format(time.RFC3339Nano) ||
		critical["loss_review_updated_at"] != reviewUpdatedAt.Format(time.RFC3339Nano) ||
		critical["facts_digest"] != plan.FactsDigest || critical["policy_version"] != float64(1) || len(critical) != 4 {
		t.Fatalf("unexpected critical projection %+v", critical)
	}
	if financial["expected_microcredits"] != float64(150) || financial["settled_microcredits"] != float64(100) ||
		financial["settlement_microcredits"] != float64(100) || financial["overage_microcredits"] != float64(50) ||
		financial["wallet_id"] != walletID.String() || financial["wallet_version"] != float64(4) ||
		financial["allocations_digest"] != plan.AllocationsDigest || len(financial) != 7 {
		t.Fatalf("unexpected financial projection %+v", financial)
	}

	rejected := &usageRepairProofFactsStub{usageRepairErr: ErrApprovalSnapshotMismatch}
	if _, reason, _, err := currentBillingProofInput(context.Background(), rejected, frozen); err != nil || reason != "DOMAIN_INVARIANT_FAILED" {
		t.Fatalf("rejected repair reason=%s err=%v", reason, err)
	}
	foreign := &usageRepairProofFactsStub{usageRepair: plan}
	foreign.usageRepair.HoldID = uuid.New()
	if _, reason, _, err := currentBillingProofInput(context.Background(), foreign, frozen); err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("foreign hold reason=%s err=%v", reason, err)
	}
	wrongEvidence := approvalencoding.LegacyProofInput{
		CommandType: usageSettlementRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		NormalizedCommand: marshalRaw(map[string]any{
			"usage_request_id": usageRequestID.String(), "hold_id": holdID.String(),
			"evidence_kind": "COMMITTED_SHORTFALL", "loss_review_id": lossReviewID.String(),
			"reason_code": usageSettlementRepairReason,
		}),
	}
	if _, _, _, err := currentBillingProofInput(context.Background(), repository, wrongEvidence); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("unsupported evidence kind err=%v", err)
	}

	now := time.Now().UTC()
	reader := &usageRepairApprovalProofStub{usageRepairProofFactsStub: repository, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	if _, err := service.billingApprovalProof(context.Background(), approvalencoding.ProofRequest{
		ApprovalRequestID: uuid.New(),
		Artifacts:         approvalencoding.ProofArtifacts{CommandType: usageSettlementRepairCommand, PolicyVersion: 1, SnapshotSchemaVersion: 1},
	}); !errors.Is(err, errBillingApprovalProofUnavailable) {
		t.Fatalf("inactive usage repair proof err=%v", err)
	}
}

// Fresh authorization requires the nonempty digest; committed result identity
// is returned separately before fresh facts checks.
func TestUsageSettlementRepairFrozenFactsDigestIsRequired(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	plan := UsageSettlementRepairPlan{
		UsageRequestID: uuid.New(), HoldID: uuid.New(), WalletID: uuid.New(), WalletVersion: 2,
		HoldUpdatedAt: now, LossReviewUpdatedAt: now, ExpectedMicrocredits: 40, SettledMicrocredits: 40,
		AllocationsDigest: strings.Repeat("1", 64), FactsDigest: strings.Repeat("2", 64),
	}
	frozen := UsageSettlementRepairFrozenFacts{HoldUpdatedAt: now, LossReviewUpdatedAt: now,
		ExpectedMicrocredits: 40, SettledMicrocredits: 40, WalletID: plan.WalletID, WalletVersion: 2,
		AllocationsDigest: plan.AllocationsDigest}
	if mismatch := usageSettlementRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("empty digest accepted: %s", mismatch)
	}
	frozen.FactsDigest = strings.Repeat("3", 64)
	if mismatch := usageSettlementRepairFrozenMismatch(plan, frozen); mismatch != "facts_digest" {
		t.Fatalf("presented digest mismatch=%q", mismatch)
	}
	frozen.FactsDigest = plan.FactsDigest
	if mismatch := usageSettlementRepairFrozenMismatch(plan, frozen); mismatch != "" {
		t.Fatalf("matching digest rejected: %s", mismatch)
	}
	frozen.AllocationsDigest = strings.Repeat("4", 64)
	if mismatch := usageSettlementRepairFrozenMismatch(plan, frozen); mismatch != "allocations_digest" {
		t.Fatalf("allocation digest mismatch=%q", mismatch)
	}
}

type usageRepairProofFactsStub struct {
	billingProofFactsStub
	usageRepair    UsageSettlementRepairPlan
	usageRepairErr error
}

func (s *usageRepairProofFactsStub) usageSettlementRepairPlan(context.Context, UsageSettlementRepairCommandInput) (UsageSettlementRepairPlan, error) {
	return s.usageRepair, s.usageRepairErr
}

type usageRepairApprovalProofStub struct {
	Repository
	*usageRepairProofFactsStub
	activation ApprovalCommandActivationSnapshot
}

func (s *usageRepairApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	return s.activation, nil
}
