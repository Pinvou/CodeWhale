package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var errBillingApprovalProofUnavailable = errors.New("authoritative Billing proof unavailable")

type billingApprovalProofRepository interface {
	billingProofFacts
	ApprovalCommandActivations(context.Context) (ApprovalCommandActivationSnapshot, error)
}

func activeBillingProofCommand(snapshot ApprovalCommandActivationSnapshot, command string, now time.Time) (ApprovalCommandActivation, error) {
	if validateApprovalCommandActivationSnapshot(snapshot, now) != nil {
		return ApprovalCommandActivation{}, errBillingApprovalProofUnavailable
	}
	for _, item := range snapshot.Activations {
		if item.CommandType == command && item.State == "ACTIVE" {
			return item, nil
		}
	}
	return ApprovalCommandActivation{}, errBillingApprovalProofUnavailable
}

func (s *Service) billingApprovalProof(ctx context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
	command := request.Artifacts.CommandType
	if command != "WALLET_ADJUSTMENT" && command != "PAYMENT_PROVIDER_RESUME" && command != "PAYMENT_PRODUCT_RESUME" && command != entitlementComponentAdjustmentCommand && command != ledgerRepairCommand && command != receivableWriteOffCommand && command != paymentFinancialRepairCommand && command != refundExceptionRepairCommand && command != chargebackFinancialRepairCommand && command != reconciliationFinancialRepairCommand && command != usageSettlementRepairCommand {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	repository, ok := s.repository.(billingApprovalProofRepository)
	if !ok {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	first, err := repository.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	active, err := activeBillingProofCommand(first, command, s.now())
	if err != nil || active.PolicyVersion != request.Artifacts.PolicyVersion || active.SnapshotSchemaVersion != request.Artifacts.SnapshotSchemaVersion {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	frozen, err := approvalencoding.LegacyProofInputFromArtifacts(request.Artifacts)
	if err != nil {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	current, reason, ownerState, err := currentBillingProofInput(ctx, repository, frozen)
	if err != nil {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	if reason == "" {
		fresh, err := approvalencoding.ConvertLegacyProofInput(current)
		if err != nil || fresh.CommandDigest != request.Artifacts.CommandDigest {
			return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
		}
		if fresh.SnapshotDigest != request.Artifacts.SnapshotDigest {
			reason = "SNAPSHOT_MISMATCH"
		}
	}
	last, err := repository.ApprovalCommandActivations(ctx)
	if err != nil {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	unchanged, err := activeBillingProofCommand(last, command, s.now())
	if err != nil || unchanged.Version != active.Version {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	now := s.now().UTC()
	response := approvalencoding.ProofResponse{
		ApprovalRequestID: request.ApprovalRequestID, CommandType: command,
		CommandDigest: request.Artifacts.CommandDigest, SnapshotDigest: request.Artifacts.SnapshotDigest,
		PolicyVersion: request.Artifacts.PolicyVersion, ProofSchemaVersion: approvalencoding.ProofSchemaVersion,
		DomainRevision: billingProofRevision(command, active.Version, current, ownerState),
		Verdict:        "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if reason != "" {
		response.Verdict, response.ReasonCode = "DENY", reason
	}
	if _, err := approvalencoding.EncodeProofResponse(response); err != nil {
		return approvalencoding.ProofResponse{}, errBillingApprovalProofUnavailable
	}
	return response, nil
}

// A revision is exclusively derived from owner facts and activation; a
// request-specific DENY reason must not change it.
func billingProofRevision(command string, activationVersion int64, current approvalencoding.LegacyProofInput, ownerState string) string {
	hash := sha256.New()
	for _, part := range []string{command, strconv.FormatInt(activationVersion, 10), string(current.CriticalResourceVersions), string(current.FinancialSnapshot), ownerState} {
		_, _ = hash.Write([]byte(part))
		_, _ = hash.Write([]byte{0})
	}
	return strconv.FormatInt(activationVersion, 10) + ":" + hex.EncodeToString(hash.Sum(nil))
}

func (s *Service) internalBillingApprovalProof(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-service" || workload.Subject == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "proof requires the Account service identity", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopClose()
	body, err := io.ReadAll(io.LimitReader(r.Body, approvalencoding.MaximumDocumentBytes+1))
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "BILLING_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || len(body) > approvalencoding.MaximumDocumentBytes {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PROOF_REQUEST", "proof request exceeds the closed transport bound", httpserver.RequestID(r.Context()))
		return
	}
	request, err := approvalencoding.DecodeProofRequest(body)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PROOF_REQUEST", "proof request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	response, err := s.billingApprovalProof(ctx, request)
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "BILLING_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_APPROVAL_PROOF_UNAVAILABLE", "authoritative proof cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	encoded, err := approvalencoding.EncodeProofResponse(response)
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "BILLING_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || !approvalencoding.ProofTransportWithinBudget(body, encoded) {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_APPROVAL_PROOF_UNAVAILABLE", "authoritative proof cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
