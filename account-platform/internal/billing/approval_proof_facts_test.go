package billing

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

type billingProofFactsStub struct {
	adjustment      ManualAdjustment
	wallet          Wallet
	provider        ProviderResumeVersions
	product         ProductResumeVersions
	confirmed       bool
	correction      entitlementCorrectionReadPlan
	ledgerRepair    LedgerRepairPlan
	ledgerRepairErr error
	writeOff        ReceivableWriteOffPlan
	writeOffErr     error
	paymentRepair   PaymentFinancialRepairPlan
	paymentRepairErr error
	refundRepair    RefundExceptionRepairPlan
	refundRepairErr error
	reads           int
}

func (s *billingProofFactsStub) refundExceptionRepairPlan(context.Context, RefundExceptionRepairCommandInput) (RefundExceptionRepairPlan, error) {
	s.reads++
	return s.refundRepair, s.refundRepairErr
}

func (s *billingProofFactsStub) paymentFinancialRepairPlan(context.Context, PaymentFinancialRepairCommandInput) (PaymentFinancialRepairPlan, error) {
	s.reads++
	return s.paymentRepair, s.paymentRepairErr
}

func (s *billingProofFactsStub) receivableWriteOffPlan(context.Context, ReceivableWriteOffCommandInput) (ReceivableWriteOffPlan, error) {
	s.reads++
	return s.writeOff, s.writeOffErr
}

func (s *billingProofFactsStub) ledgerRepairPlan(context.Context, LedgerRepairCommandInput) (LedgerRepairPlan, error) {
	s.reads++
	return s.ledgerRepair, s.ledgerRepairErr
}

func (s *billingProofFactsStub) AdjustmentForExecution(context.Context, uuid.UUID) (ManualAdjustment, error) {
	s.reads++
	return s.adjustment, nil
}
func (s *billingProofFactsStub) GetWallet(context.Context, uuid.UUID) (Wallet, error) {
	s.reads++
	return s.wallet, nil
}
func (s *billingProofFactsStub) providerResumeVersions(context.Context, ProviderResumeVersions) (ProviderResumeVersions, error) {
	s.reads++
	return s.provider, nil
}
func (s *billingProofFactsStub) productResumeVersions(context.Context, ProductResumeVersions) (ProductResumeVersions, error) {
	s.reads++
	return s.product, nil
}
func (s *billingProofFactsStub) confirmedResumeProbe(context.Context, string, string, string, uuid.UUID) (bool, error) {
	s.reads++
	return s.confirmed, nil
}
func (s *billingProofFactsStub) entitlementCorrectionPlan(context.Context, uuid.UUID, uuid.UUID) (entitlementCorrectionReadPlan, error) {
	s.reads++
	return s.correction, nil
}

func TestEntitlementCorrectionProofFactsBindOwnerFacts(t *testing.T) {
	marshal := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	packageID, orderID := uuid.New(), uuid.New()
	walletID, transactionID := uuid.New(), uuid.New()
	sourceComponent, sourceLot := uuid.New(), uuid.New()
	stub := &billingProofFactsStub{correction: entitlementCorrectionReadPlan{
		PackageID: packageID, WalletID: walletID, PaymentOrderID: orderID, PaymentTransactionID: transactionID,
		SourceComponentID: sourceComponent, SourceLotID: sourceLot, WalletVersion: 3, PackageTotal: 100,
		SourceOriginal: 70, TargetOriginal: 30,
		SourceEvidenceDigest: strings.Repeat("ab", 32), FactsDigest: strings.Repeat("cd", 32),
		Amounts: entitlementCorrectionAmounts{SourceType: "BASE", TargetType: "BONUS", Amount: 20, ExpectedBase: 50, ExpectedBonus: 50, CurrentBase: 70, CurrentBonus: 30},
	}}
	frozen := approvalencoding.LegacyProofInput{
		CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: packageID.String(),
		NormalizedCommand:        marshal(map[string]any{"package_id": packageID, "payment_order_id": orderID, "reason_code": "SNAPSHOT_SPLIT_MISMATCH"}),
		CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`),
	}
	current, reason, ownerState, err := currentBillingProofInput(context.Background(), stub, frozen)
	if err != nil || reason != "" || ownerState != "BASE->BONUS" || stub.reads != 1 {
		t.Fatalf("correction facts reason=%s owner=%s reads=%d err=%v", reason, ownerState, stub.reads, err)
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(current)
	if err != nil {
		t.Fatalf("correction projection must satisfy the closed v2 schema: %v", err)
	}
	if artifacts.PolicyVersion != 2 || artifacts.SnapshotSchemaVersion != 2 || artifacts.CommandDigest == "" || artifacts.SnapshotDigest == "" {
		t.Fatalf("correction projection artifacts=%+v", artifacts)
	}
	drifted := stub.correction
	drifted.Amounts.Amount = 0
	stub.correction = drifted
	driftedInput, _, _, err := currentBillingProofInput(context.Background(), stub, frozen)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approvalencoding.ConvertLegacyProofInput(driftedInput); err == nil {
		t.Fatal("a zero recomputed amount must not satisfy the closed v2 schema")
	}
}

func TestBillingProofFactsUsePersistedOwnerRows(t *testing.T) {
	marshal := func(value any) json.RawMessage {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	adjustment := ManualAdjustment{ID: uuid.New(), WalletID: uuid.New(), AmountMicrocredits: 730, RequestedBy: uuid.New(), Version: 2, Status: "PENDING_APPROVAL"}
	wallet := Wallet{ID: adjustment.WalletID, Status: "ACTIVE"}
	stub := &billingProofFactsStub{adjustment: adjustment, wallet: wallet}
	command := map[string]any{"adjustment_id": adjustment.ID, "wallet_id": adjustment.WalletID, "amount_microcredits": adjustment.AmountMicrocredits, "requested_by": adjustment.RequestedBy}
	frozen := approvalencoding.LegacyProofInput{CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustment.ID.String(), NormalizedCommand: marshal(command), CriticalResourceVersions: marshal(map[string]any{"adjustment_version": 2}), FinancialSnapshot: marshal(map[string]any{"wallet_id": adjustment.WalletID, "delta_microcredits": 730})}
	current, reason, _, err := currentBillingProofInput(context.Background(), stub, frozen)
	if err != nil || reason != "" || stub.reads != 2 || !json.Valid(current.FinancialSnapshot) {
		t.Fatalf("wallet proof facts=%+v reason=%s reads=%d err=%v", current, reason, stub.reads, err)
	}
	if _, err := approvalencoding.ConvertLegacyProofInput(current); err != nil {
		t.Fatalf("wallet projection must satisfy the closed proof schema: %v", err)
	}
	stub.adjustment.Version++
	current, reason, _, err = currentBillingProofInput(context.Background(), stub, frozen)
	if err != nil || reason != "" || string(current.CriticalResourceVersions) != `{"adjustment_version":3}` {
		t.Fatalf("changed owner version=%s reason=%s err=%v", current.CriticalResourceVersions, reason, err)
	}
	stub.adjustment.Status = "POSTED"
	_, reason, _, err = currentBillingProofInput(context.Background(), stub, frozen)
	if err != nil || reason != "RESOURCE_STATE_CHANGED" {
		t.Fatalf("posted wallet adjustment reason=%s err=%v", reason, err)
	}

	probeID := uuid.New()
	provider := ProviderResumeVersions{ProviderID: "provider-a", ProviderVersion: "v1", ProviderControlVersion: 3, ProbeOrderID: probeID, PolicyVersion: "p1", ActiveMappingSetFingerprint: strings.Repeat("a", 32)}
	stub.provider, stub.confirmed = provider, true
	providerFrozen := approvalencoding.LegacyProofInput{CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: provider.ProviderID, NormalizedCommand: marshal(provider), CriticalResourceVersions: marshal(provider), FinancialSnapshot: marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": 3, "product_control_version": nil})}
	current, reason, _, err = currentBillingProofInput(context.Background(), stub, providerFrozen)
	if err != nil || reason != "" {
		t.Fatalf("provider persisted facts reason=%s err=%v", reason, err)
	}
	if _, err := approvalencoding.ConvertLegacyProofInput(current); err != nil {
		t.Fatalf("provider projection must satisfy the closed proof schema: %v", err)
	}
	stub.confirmed = false
	_, reason, _, err = currentBillingProofInput(context.Background(), stub, providerFrozen)
	if err != nil || reason != "EVIDENCE_MISSING" {
		t.Fatalf("missing persisted provider evidence reason=%s err=%v", reason, err)
	}

	product := ProductResumeVersions{ProviderID: provider.ProviderID, ProviderVersion: provider.ProviderVersion, ProductID: "product-a", ProviderControlVersion: 3, ProductControlVersion: 4, CatalogItemID: uuid.New(), CatalogVersion: 5, AdmissionVersion: 6, MappingID: uuid.New(), MappingVersion: 7, ProbeOrderID: probeID, PolicyVersion: "p1"}
	stub.product, stub.confirmed = product, true
	productFrozen := approvalencoding.LegacyProofInput{CommandType: "PAYMENT_PRODUCT_RESUME", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PAYMENT_PRODUCT_CONTROL", TargetResourceID: product.ProductID, NormalizedCommand: marshal(product), CriticalResourceVersions: marshal(product), FinancialSnapshot: marshal(map[string]any{"new_orders_enabled": false, "provider_control_version": 3, "product_control_version": 4})}
	current, reason, _, err = currentBillingProofInput(context.Background(), stub, productFrozen)
	if err != nil || reason != "" {
		t.Fatalf("product persisted facts reason=%s err=%v", reason, err)
	}
	if _, err := approvalencoding.ConvertLegacyProofInput(current); err != nil {
		t.Fatalf("product projection must satisfy the closed proof schema: %v", err)
	}
	stub.product.ProductControlVersion++
	_, reason, _, err = currentBillingProofInput(context.Background(), stub, productFrozen)
	if err != nil || reason != "SNAPSHOT_MISMATCH" {
		t.Fatalf("product version drift reason=%s err=%v", reason, err)
	}
}
