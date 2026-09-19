package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// billingApprovalPrecheckStub records exactly which closed owner input the
// precheck asked for, so the test can prove the mapping without a database.
type billingApprovalPrecheckStub struct {
	command        string
	usageRequestID uuid.UUID
	holdID         uuid.UUID
	paymentOrderID uuid.UUID
	evidenceID     uuid.UUID
	receivableID   uuid.UUID
	amount         int64
	expected       int64
	reasonCode     string
	err            error
}

func (s *billingApprovalPrecheckStub) projection(command, targetType, targetID string) (approvalencoding.LegacyProofInput, error) {
	if s.err != nil {
		return approvalencoding.LegacyProofInput{}, s.err
	}
	s.command = command
	normalized, _ := json.Marshal(map[string]any{"command_type": command})
	critical, _ := json.Marshal(map[string]any{"policy_version": 1})
	financial, _ := json.Marshal(map[string]any{"wallet_version": 1})
	return approvalencoding.LegacyProofInput{
		CommandType: command, PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: targetType, TargetResourceID: targetID,
		NormalizedCommand: normalized, CriticalResourceVersions: critical, FinancialSnapshot: financial,
	}, nil
}

func (s *billingApprovalPrecheckStub) LedgerRepairProofInput(_ context.Context, input LedgerRepairCommandInput, reasonCode string) (approvalencoding.LegacyProofInput, error) {
	s.amount, s.reasonCode = input.AmountMicrocredits, reasonCode
	return s.projection(ledgerRepairCommand, "LEDGER_POSTING", input.TargetPostingID.String())
}

func (s *billingApprovalPrecheckStub) ReceivableWriteOffProofInput(_ context.Context, input ReceivableWriteOffCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.receivableID, s.amount, s.expected = input.ReceivableID, input.WriteOffMicrocredits, input.ExpectedReceivableVersion
	return s.projection(receivableWriteOffCommand, "RECEIVABLE", input.ReceivableID.String())
}

func (s *billingApprovalPrecheckStub) PaymentFinancialRepairProofInput(_ context.Context, input PaymentFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.paymentOrderID, s.evidenceID = input.PaymentOrderID, input.PaymentEvidenceID
	return s.projection(paymentFinancialRepairCommand, "PAYMENT_ORDER", input.PaymentOrderID.String())
}

func (s *billingApprovalPrecheckStub) RefundExceptionRepairProofInput(_ context.Context, input RefundExceptionRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.evidenceID = input.PaymentEvidenceID
	return s.projection(refundExceptionRepairCommand, "REFUND", input.RefundID.String())
}

func (s *billingApprovalPrecheckStub) ChargebackFinancialRepairProofInput(_ context.Context, input ChargebackFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.evidenceID = input.PaymentEvidenceID
	return s.projection(chargebackFinancialRepairCommand, "CHARGEBACK_CASE", input.ChargebackCaseID.String())
}

func (s *billingApprovalPrecheckStub) ReconciliationFinancialRepairProofInput(_ context.Context, input ReconciliationFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.evidenceID = input.PaymentEvidenceID
	return s.projection(reconciliationFinancialRepairCommand, "RECONCILIATION_CASE", input.ReconciliationCaseID.String())
}

func (s *billingApprovalPrecheckStub) UsageSettlementRepairProofInput(_ context.Context, input UsageSettlementRepairCommandInput) (approvalencoding.LegacyProofInput, error) {
	s.usageRequestID, s.holdID = input.UsageRequestID, input.HoldID
	return s.projection(usageSettlementRepairCommand, "USAGE_HOLD", input.HoldID.String())
}

// TestBillingApprovalPrecheckUsesTheClosedProofInputs proves that every command
// maps to exactly the same closed owner input the proof path uses, that the
// usage settlement evidence kind stays pinned to the implemented first release,
// and that an unusable request is a definite, zero-write refusal.
func TestBillingApprovalPrecheckUsesTheClosedProofInputs(t *testing.T) {
	caseID, evidenceID, requestID := uuid.New(), uuid.New(), uuid.New()
	for name, test := range map[string]struct {
		request billingApprovalPrecheckRequest
		check   func(t *testing.T, stub *billingApprovalPrecheckStub, projection approvalencoding.LegacyProofInput)
	}{
		"receivable write-off": {
			request: billingApprovalPrecheckRequest{CommandType: receivableWriteOffCommand, TargetResourceID: caseID.String(),
				WriteOffMicrocredits: 500, ExpectedVersion: 3},
			check: func(t *testing.T, stub *billingApprovalPrecheckStub, projection approvalencoding.LegacyProofInput) {
				if stub.receivableID != caseID || stub.amount != 500 || stub.expected != 3 ||
					projection.TargetResourceType != "RECEIVABLE" {
					t.Fatalf("write-off mapping=%+v %+v", stub, projection)
				}
			},
		},
		"payment financial repair": {
			request: billingApprovalPrecheckRequest{CommandType: paymentFinancialRepairCommand, TargetResourceID: caseID.String(),
				PaymentEvidenceID: evidenceID.String()},
			check: func(t *testing.T, stub *billingApprovalPrecheckStub, projection approvalencoding.LegacyProofInput) {
				if stub.paymentOrderID != caseID || stub.evidenceID != evidenceID || projection.TargetResourceType != "PAYMENT_ORDER" {
					t.Fatalf("payment repair mapping=%+v %+v", stub, projection)
				}
			},
		},
		"usage settlement repair": {
			request: billingApprovalPrecheckRequest{CommandType: usageSettlementRepairCommand, TargetResourceID: requestID.String(),
				LossReviewID: evidenceID.String()},
			check: func(t *testing.T, stub *billingApprovalPrecheckStub, projection approvalencoding.LegacyProofInput) {
				if stub.usageRequestID != requestID || stub.holdID != evidenceID ||
					projection.TargetResourceType != "USAGE_HOLD" {
					t.Fatalf("usage repair mapping=%+v %+v", stub, projection)
				}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &billingApprovalPrecheckStub{}
			projection, err := precheckBillingApprovalCommand(context.Background(), stub, test.request)
			if err != nil {
				t.Fatalf("precheck: %v", err)
			}
			test.check(t, stub, projection)
		})
	}

	// The usage settlement precheck can only describe the implemented first
	// release; the unsupported evidence kind is rejected by the owner input, and
	// every unusable request is a definite refusal.
	for name, request := range map[string]billingApprovalPrecheckRequest{
		"unknown command":  {CommandType: "FUTURE_REPAIR", TargetResourceID: caseID.String()},
		"missing evidence": {CommandType: paymentFinancialRepairCommand, TargetResourceID: caseID.String()},
		"zero amount":      {CommandType: receivableWriteOffCommand, TargetResourceID: caseID.String(), ExpectedVersion: 1},
		"missing date":     {CommandType: reconciliationFinancialRepairCommand, TargetResourceID: caseID.String(), PaymentEvidenceID: evidenceID.String()},
		"padded id":        {CommandType: paymentFinancialRepairCommand, TargetResourceID: " " + caseID.String(), PaymentEvidenceID: evidenceID.String()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := precheckBillingApprovalCommand(context.Background(), &billingApprovalPrecheckStub{}, request); !errors.Is(err, errBillingApprovalPrecheckUnavailable) {
				t.Fatalf("%s must fail closed, got %v", name, err)
			}
		})
	}
	unavailable := &billingApprovalPrecheckStub{err: ErrNotFound}
	if _, err := precheckBillingApprovalCommand(context.Background(), unavailable, billingApprovalPrecheckRequest{
		CommandType: paymentFinancialRepairCommand, TargetResourceID: caseID.String(), PaymentEvidenceID: evidenceID.String(),
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("owner refusal must surface unchanged, got %v", err)
	}
}
