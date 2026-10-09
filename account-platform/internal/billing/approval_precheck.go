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
	"unicode/utf8"

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
	EntitlementCorrectionProofInput(context.Context, uuid.UUID, uuid.UUID, string) (approvalencoding.LegacyProofInput, error)
}

// billingApprovalPrecheckRequest is the closed request the Account service may
// send. The decoder rejects unknown fields, so a caller cannot smuggle extra
// facts into the owner read.
type billingApprovalPrecheckRequest struct {
	PaymentEvidenceID      string `json:"payment_evidence_id,omitempty"`
	CommandType            string `json:"command_type"`
	TargetResourceID       string `json:"target_resource_id"`
	PaymentOrderID         string `json:"payment_order_id,omitempty"`
	ReasonCode             string `json:"reason_code"`
	TargetJournalEntryID   string `json:"target_journal_entry_id,omitempty"`
	WrongLedgerAccountID   string `json:"wrong_ledger_account_id,omitempty"`
	CorrectLedgerAccountID string `json:"correct_ledger_account_id,omitempty"`
	AmountMicrocredits     int64  `json:"amount_microcredits,omitempty"`
	WriteOffMicrocredits   int64  `json:"write_off_microcredits,omitempty"`
	ExpectedVersion        int64  `json:"expected_version,omitempty"`
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
	switch request.CommandType {
	case "ENTITLEMENT_COMPONENT_ADJUSTMENT":
		packageID, packageOK := validPrecheckUUID(request.TargetResourceID)
		orderID, orderOK := validPrecheckUUID(request.PaymentOrderID)
		if !packageOK || !orderOK || request.ReasonCode == "" || strings.TrimSpace(request.ReasonCode) != request.ReasonCode || len(request.ReasonCode) > 80 || request.TargetJournalEntryID != "" || request.WrongLedgerAccountID != "" || request.CorrectLedgerAccountID != "" || request.AmountMicrocredits != 0 || request.WriteOffMicrocredits != 0 || request.ExpectedVersion != 0 || request.PaymentEvidenceID != "" {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return repository.EntitlementCorrectionProofInput(ctx, packageID, orderID, request.ReasonCode)
	case ledgerRepairCommand:
		journal, journalOK := validPrecheckUUID(request.TargetJournalEntryID)
		posting, postingOK := validPrecheckUUID(request.TargetResourceID)
		wrong, wrongOK := validPrecheckUUID(request.WrongLedgerAccountID)
		correct, correctOK := validPrecheckUUID(request.CorrectLedgerAccountID)
		if !journalOK || !postingOK || !wrongOK || !correctOK || wrong == correct || request.AmountMicrocredits < 1 || request.PaymentOrderID != "" || request.ReasonCode != "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT" || request.WriteOffMicrocredits != 0 || request.ExpectedVersion != 0 || request.PaymentEvidenceID != "" {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		owner, ok := repository.(interface {
			LedgerRepairProofInput(context.Context, LedgerRepairCommandInput, string) (approvalencoding.LegacyProofInput, error)
		})
		if !ok {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return owner.LedgerRepairProofInput(ctx, LedgerRepairCommandInput{TargetJournalEntryID: journal, TargetPostingID: posting, WrongLedgerAccountID: wrong, CorrectLedgerAccountID: correct, AmountMicrocredits: request.AmountMicrocredits}, request.ReasonCode)
	case receivableWriteOffCommand:
		receivableID, ok := validPrecheckUUID(request.TargetResourceID)
		if !ok || request.WriteOffMicrocredits < 1 || request.ExpectedVersion < 1 || request.ReasonCode != receivableWriteOffReason || request.PaymentOrderID != "" || request.TargetJournalEntryID != "" || request.WrongLedgerAccountID != "" || request.CorrectLedgerAccountID != "" || request.AmountMicrocredits != 0 || request.PaymentEvidenceID != "" {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		owner, ok := repository.(interface {
			ReceivableWriteOffProofInput(context.Context, ReceivableWriteOffCommandInput) (approvalencoding.LegacyProofInput, error)
		})
		if !ok {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return owner.ReceivableWriteOffProofInput(ctx, ReceivableWriteOffCommandInput{ReceivableID: receivableID, WriteOffMicrocredits: request.WriteOffMicrocredits, ExpectedReceivableVersion: request.ExpectedVersion})
	case paymentFinancialRepairCommand:
		orderID, orderOK := validPrecheckUUID(request.TargetResourceID)
		evidenceID, evidenceOK := validPrecheckUUID(request.PaymentEvidenceID)
		if !orderOK || !evidenceOK || request.ReasonCode != paymentFinancialRepairReason || request.PaymentOrderID != "" || request.TargetJournalEntryID != "" || request.WrongLedgerAccountID != "" || request.CorrectLedgerAccountID != "" || request.AmountMicrocredits != 0 || request.WriteOffMicrocredits != 0 || request.ExpectedVersion != 0 {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		owner, ok := repository.(interface {
			PaymentFinancialRepairProofInput(context.Context, PaymentFinancialRepairCommandInput) (approvalencoding.LegacyProofInput, error)
		})
		if !ok {
			return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
		}
		return owner.PaymentFinancialRepairProofInput(ctx, PaymentFinancialRepairCommandInput{
			PaymentOrderID: orderID, PaymentEvidenceID: evidenceID,
		})
	}
	return approvalencoding.LegacyProofInput{}, errBillingApprovalPrecheckUnavailable
}

// The owner accepts one complete object with unique keys. It never accepts
// caller financial facts or a second trailing JSON document.
func decodeBillingApprovalPrecheckRequest(raw []byte) (billingApprovalPrecheckRequest, error) {
	var result billingApprovalPrecheckRequest
	if !utf8.Valid(raw) {
		return result, errBillingApprovalPrecheckUnavailable
	}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	opening, err := tokens.Token()
	if err != nil || opening != json.Delim('{') {
		return result, errBillingApprovalPrecheckUnavailable
	}
	seen := make(map[string]bool)
	for tokens.More() {
		token, err := tokens.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return result, errBillingApprovalPrecheckUnavailable
		}
		seen[key] = true
		var value json.RawMessage
		if tokens.Decode(&value) != nil {
			return result, errBillingApprovalPrecheckUnavailable
		}
	}
	if closing, err := tokens.Token(); err != nil || closing != json.Delim('}') {
		return result, errBillingApprovalPrecheckUnavailable
	}
	if tokens.Decode(new(any)) != io.EOF {
		return result, errBillingApprovalPrecheckUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return billingApprovalPrecheckRequest{}, errBillingApprovalPrecheckUnavailable
	}
	return result, nil
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
	request, err := decodeBillingApprovalPrecheckRequest(body)
	if err != nil {
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
