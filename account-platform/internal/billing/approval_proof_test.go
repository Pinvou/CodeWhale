package billing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

type billingApprovalProofStub struct {
	Repository
	*billingProofFactsStub
	activation ApprovalCommandActivationSnapshot
	calls      int
}

func (s *billingApprovalProofStub) GetWallet(ctx context.Context, id uuid.UUID) (Wallet, error) {
	return s.billingProofFactsStub.GetWallet(ctx, id)
}

func (s *billingApprovalProofStub) ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error) {
	s.calls++
	return s.activation, nil
}

func TestBillingApprovalProofRechecksActivationAndOwnerFacts(t *testing.T) {
	now := time.Now().UTC()
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 370, RequestedBy: uuid.New(), Version: 1, Status: "PENDING_APPROVAL"}
	reader := &billingApprovalProofStub{billingProofFactsStub: &billingProofFactsStub{adjustment: adjustment, wallet: Wallet{ID: adjustment.WalletID, Status: "ACTIVE"}}, activation: validBillingApprovalActivationSnapshot(now)}
	service := NewService(reader)
	marshal := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	input := approvalencoding.LegacyProofInput{
		CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(),
		NormalizedCommand:        marshal(map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy}),
		CriticalResourceVersions: marshal(map[string]any{"adjustment_version": 1}),
		FinancialSnapshot:        marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": adjustment.AmountMicrocredits}),
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(input)
	if err != nil {
		t.Fatal(err)
	}
	request := approvalencoding.ProofRequest{ApprovalRequestID: uuid.New(), Artifacts: artifacts}
	proof, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || proof.Verdict != "ALLOW" || proof.ReasonCode != "PROOF_VALID" || reader.calls != 2 {
		t.Fatalf("authoritative wallet proof=%+v calls=%d err=%v", proof, reader.calls, err)
	}
	if _, err := approvalencoding.DeriveProofDigest(proof); err != nil {
		t.Fatalf("response must meet the closed transport: %v", err)
	}
	reader.adjustment.Version++
	changed, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || changed.Verdict != "DENY" || changed.ReasonCode != "SNAPSHOT_MISMATCH" || changed.DomainRevision == proof.DomainRevision {
		t.Fatalf("owner version drift=%+v err=%v", changed, err)
	}
	reader.adjustment.Version--
	reader.adjustment.Status = "POSTED"
	posted, err := service.billingApprovalProof(context.Background(), request)
	if err != nil || posted.Verdict != "DENY" || posted.ReasonCode != "RESOURCE_STATE_CHANGED" || posted.DomainRevision == proof.DomainRevision {
		t.Fatalf("owner status drift=%+v err=%v", posted, err)
	}
	for i := range reader.activation.Activations {
		if reader.activation.Activations[i].CommandType == "WALLET_ADJUSTMENT" {
			reader.activation.Activations[i].State = "INACTIVE"
			reader.activation.Activations[i].Version++
		}
	}
	if _, err := service.billingApprovalProof(context.Background(), request); err == nil {
		t.Fatal("inactive Billing activation issued a proof")
	}
}
