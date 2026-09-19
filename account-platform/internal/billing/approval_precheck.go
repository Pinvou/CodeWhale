package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

// billingApprovalPrecheckRepository is the read-only owner surface the precheck
// endpoint needs: exactly the same closed projections the proof path assembles,
// so a precheck can never be more permissive than the proof it precedes.
type billingApprovalPrecheckRepository interface {
	LedgerRepairProofInput(context.Context, LedgerRepairCommandInput, string) (approvalencoding.LegacyProofInput, error)
	ReceivableWriteOffProofInput(context.Context, ReceivableWriteOffCommandInput) (approvalencoding.LegacyProofInput, error)
	PaymentFinancialRepairProofInput(context.Context, PaymentFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error)
	RefundExceptionRepairProofInput(context.Context, RefundExceptionRepairCommandInput) (approvalencoding.LegacyProofInput, error)
	ChargebackFinancialRepairProofInput(context.Context, ChargebackFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error)
	ReconciliationFinancialRepairProofInput(context.Context, ReconciliationFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error)
	UsageSettlementRepairProofInput(context.Context, UsageSettlementRepairCommandInput) (approvalencoding.LegacyProofInput, error)
}

// billingApprovalPrecheckRequest is the closed request the Account service may
// send. The decoder rejects unknown fields, so a caller cannot smuggle extra
// facts into the owner read.
type billingApprovalPrecheckRequest struct {
	CommandType            string `json:"command_type"`
	TargetResourceID       string `json:"target_resource_id"`
	TargetJournalEntryID   string `json:"target_journal_entry_id"`
	PaymentEvidenceID      string `json:"payment_evidence_id"`
	AnomalyCaseID          string `json:"anomaly_case_id"`
	LossReviewID           string `json:"loss_review_id"`
	BusinessDate           string `json:"business_date"`
	ReasonCode             string `json:"reason_code"`
	ExpectedVersion        int64  `json:"expected_version"`
	WriteOffMicrocredits   int64  `json:"write_off_microcredits"`
	WrongLedgerAccountID   string `json:"wrong_ledger_account_id"`
	CorrectLedgerAccountID string `json:"correct_ledger_account_id"`
	AmountMicrocredits     int64  `json:"amount_microcredits"`
}

// billingApprovalPrecheckResponse is the closed projection the Portal renders.
// It carries the owner-derived normalized/critical/financial documents verbatim
// and never any Provider text, prompt or personal data.
type billingApprovalPrecheckResponse struct {
	CommandType              string          `json:"command_type"`
	PolicyVersion            int64           `json:"policy_version"`
	SnapshotSchemaVersion    int             `json:"snapshot_schema_version"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	NormalizedCommand        json.RawMessage `json:"normalized_command"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
}

// errBillingApprovalPrecheckUnavailable marks a defined, zero-write refusal: the
// caller gets a closed failure code instead of a projection that the proof would
// have rejected anyway.
var errBillingApprovalPrecheckUnavailable = errors.New("billing approval precheck unavailable")

func validPrecheckUUID(value string) (uuid.UUID, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || trimmed != value {
		return uuid.Nil, false
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil || parsed == uuid.Nil {
		return uuid.Nil, false
	}
	return parsed, true
}

// precheck assembles the closed owner projection for exactly one command. Every
// branch uses the same ProofInput the proof authority uses, so the two can never
// disagree about the frozen facts.
func precheckBillingApprovalCommand(ctx context.Context, repository billingApprovalPrecheckRepository, request billingApprovalPrecheckRequest) (approvalencoding.LegacyProofInput, error) {
	switch strings.TrimSpace(request.CommandType) {
	case ledgerRepairCommand:
		journalID, journalOK := validPrecheckUUID(request.TargetJournalEntryID)
		postingID, postingOK := validPrecheckUUID(request.TargetResourceID)
		wrongAccount, wrongOK := validPrecheckUUID(request.WrongLedgerAccountID)
		correctAccount, correctOK := validPrecheckUUID(request.CorrectLedgerAccountID)
		if !journalOK || !postingOK || !wrongOK || !correctOK || request.AmountMicrocredits <= 0 {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.LedgerRepairProofInput(ctx, LedgerRepairCommandInput{
			TargetJournalEntryID: journalID, TargetPostingID: postingID,
			WrongLedgerAccountID: wrongAccount, CorrectLedgerAccountID: correctAccount,
			AmountMicrocredits: request.AmountMicrocredits,
		}, strings.TrimSpace(request.ReasonCode))
	case receivableWriteOffCommand:
		receivableID, ok := validPrecheckUUID(request.TargetResourceID)
		if !ok || request.WriteOffMicrocredits <= 0 || request.ExpectedVersion <= 0 {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.ReceivableWriteOffProofInput(ctx, ReceivableWriteOffCommandInput{
			ReceivableID: receivableID, WriteOffMicrocredits: request.WriteOffMicrocredits,
			ExpectedReceivableVersion: request.ExpectedVersion,
		})
	case paymentFinancialRepairCommand:
		orderID, orderOK := validPrecheckUUID(request.TargetResourceID)
		evidenceID, evidenceOK := validPrecheckUUID(request.PaymentEvidenceID)
		if !orderOK || !evidenceOK {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.PaymentFinancialRepairProofInput(ctx, PaymentFinancialRepairCommandInput{
			PaymentOrderID: orderID, PaymentEvidenceID: evidenceID,
		})
	case refundExceptionRepairCommand:
		refundID, refundOK := validPrecheckUUID(request.TargetResourceID)
		evidenceID, evidenceOK := validPrecheckUUID(request.PaymentEvidenceID)
		anomalyID, anomalyOK := validPrecheckUUID(request.AnomalyCaseID)
		if !refundOK || !evidenceOK || !anomalyOK {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.RefundExceptionRepairProofInput(ctx, RefundExceptionRepairCommandInput{
			RefundID: refundID, PaymentEvidenceID: evidenceID, AnomalyCaseID: anomalyID,
		})
	case chargebackFinancialRepairCommand:
		caseID, caseOK := validPrecheckUUID(request.TargetResourceID)
		evidenceID, evidenceOK := validPrecheckUUID(request.PaymentEvidenceID)
		if !caseOK || !evidenceOK {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.ChargebackFinancialRepairProofInput(ctx, ChargebackFinancialRepairCommandInput{
			ChargebackCaseID: caseID, PaymentEvidenceID: evidenceID,
		})
	case reconciliationFinancialRepairCommand:
		caseID, caseOK := validPrecheckUUID(request.TargetResourceID)
		evidenceID, evidenceOK := validPrecheckUUID(request.PaymentEvidenceID)
		if !caseOK || !evidenceOK || strings.TrimSpace(request.BusinessDate) == "" {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.ReconciliationFinancialRepairProofInput(ctx, ReconciliationFinancialRepairCommandInput{
			ReconciliationCaseID: caseID, PaymentEvidenceID: evidenceID, BusinessDate: request.BusinessDate,
		})
	case usageSettlementRepairCommand:
		requestID, requestOK := validPrecheckUUID(request.TargetResourceID)
		holdID, holdOK := validPrecheckUUID(request.LossReviewID)
		if !requestOK || !holdOK {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.UsageSettlementRepairProofInput(ctx, UsageSettlementRepairCommandInput{
			UsageRequestID: requestID, HoldID: holdID,
			EvidenceKind: usageSettlementRepairEvidenceKind, LossReviewID: holdID,
		})
	default:
		return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
	}
}

// internalBillingApprovalPrecheck serves the read-only closed projection the
// Portal needs before it assembles an approval draft. It is Account-service only,
// bounded by a two second deadline and the same 64 KiB document bound as the
// proof transport, and it writes no fact and issues no claim.
func (s *Service) internalBillingApprovalPrecheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-service" || workload.Subject == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "precheck requires the Account service identity", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopClose()
	body, err := io.ReadAll(io.LimitReader(r.Body, approvalencoding.MaximumDocumentBytes+1))
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "BILLING_APPROVAL_PRECHECK_TIMEOUT", "precheck deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || len(body) > approvalencoding.MaximumDocumentBytes {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST", "precheck request exceeds the closed transport bound", httpserver.RequestID(r.Context()))
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request billingApprovalPrecheckRequest
	if err := decoder.Decode(&request); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PRECHECK_REQUEST", "precheck request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(billingApprovalPrecheckRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_APPROVAL_PRECHECK_UNAVAILABLE", "authoritative precheck cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	projection, err := precheckBillingApprovalCommand(ctx, repository, request)
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "BILLING_APPROVAL_PRECHECK_TIMEOUT", "precheck deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusUnprocessableEntity, "BILLING_APPROVAL_PRECHECK_UNAVAILABLE", "the closed owner projection is not available for this target", httpserver.RequestID(r.Context()))
		return
	}
	response := billingApprovalPrecheckResponse{
		CommandType: projection.CommandType, PolicyVersion: projection.PolicyVersion,
		SnapshotSchemaVersion: projection.SnapshotSchemaVersion,
		TargetResourceType:    projection.TargetResourceType, TargetResourceID: projection.TargetResourceID,
		NormalizedCommand: projection.NormalizedCommand, CriticalResourceVersions: projection.CriticalResourceVersions,
		FinancialSnapshot: projection.FinancialSnapshot,
	}
	encoded, err := json.Marshal(response)
	if err != nil || len(encoded)+len(body) > approvalencoding.MaximumDocumentBytes {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_APPROVAL_PRECHECK_UNAVAILABLE", "authoritative precheck cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
