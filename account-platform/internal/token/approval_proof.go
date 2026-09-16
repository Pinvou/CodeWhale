package token

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

var errTokenApprovalProofUnavailable = errors.New("authoritative Token proof unavailable")

func (s *Service) internalTokenApprovalProof(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-service" || workload.Subject == "" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "proof requires the Account service identity", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	body, err := io.ReadAll(io.LimitReader(r.Body, approvalencoding.MaximumDocumentBytes+1))
	if err != nil || len(body) > approvalencoding.MaximumDocumentBytes || ctx.Err() != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PROOF_REQUEST", "proof request exceeds the closed transport bound", httpserver.RequestID(r.Context()))
		return
	}
	request, err := approvalencoding.DecodeProofRequest(body)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_PROOF_REQUEST", "proof request is invalid", httpserver.RequestID(r.Context()))
		return
	}
	response, err := s.tokenApprovalProof(ctx, request)
	if err != nil || ctx.Err() != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_APPROVAL_PROOF_UNAVAILABLE", "authoritative proof cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	encoded, err := approvalencoding.EncodeProofResponse(response)
	if err != nil || !approvalencoding.ProofTransportWithinBudget(body, encoded) || ctx.Err() != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_APPROVAL_PROOF_UNAVAILABLE", "authoritative proof cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// tokenApprovalProof recomputes both digests from owner-domain facts. A
// mismatched frozen snapshot is a DENY, never an authorization to refresh it.
// The revision is a stable fingerprint of the actual owner precheck together
// with the Token activation epoch; execution must revalidate both locally.
func (s *Service) tokenApprovalProof(ctx context.Context, request approvalencoding.ProofRequest) (approvalencoding.ProofResponse, error) {
	artifacts := request.Artifacts
	switch artifacts.CommandType {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE":
	default:
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	if s.commandGovernanceActivation == nil {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	first, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx)
	if err != nil || first.State != "ACTIVE" || first.Epoch < 1 {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	input, err := approvalencoding.LegacyProofInputFromArtifacts(artifacts)
	if err != nil {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	current, err := s.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{
		CommandType: input.CommandType, TargetResourceType: input.TargetResourceType,
		TargetResourceID: input.TargetResourceID, NormalizedCommand: input.NormalizedCommand,
	})
	if err != nil || !validImpactFingerprint(current.PrecheckFingerprint) {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	fresh, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: input.CommandType, PolicyVersion: input.PolicyVersion,
		SnapshotSchemaVersion: input.SnapshotSchemaVersion,
		TargetResourceType:    input.TargetResourceType, TargetResourceID: input.TargetResourceID,
		NormalizedCommand:        input.NormalizedCommand,
		CriticalResourceVersions: current.CriticalResourceVersions, FinancialSnapshot: current.FinancialSnapshot,
	})
	if err != nil || fresh.CommandDigest != artifacts.CommandDigest {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	last, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx)
	if err != nil || last.State != "ACTIVE" || last.Epoch != first.Epoch {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	now := time.Now().UTC()
	response := approvalencoding.ProofResponse{
		ApprovalRequestID: request.ApprovalRequestID, CommandType: artifacts.CommandType,
		CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
		PolicyVersion: artifacts.PolicyVersion, ProofSchemaVersion: approvalencoding.ProofSchemaVersion,
		DomainRevision: strconv.FormatInt(last.Epoch, 10) + ":" + current.PrecheckFingerprint,
		Verdict:        "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	// billing_as_of is observation metadata and advances on every independent
	// Billing read. It is frozen in the original proof digest, but a new read
	// must compare the price facts rather than falsely deny elapsed time.
	if !tokenProofSnapshotMatches(artifacts.CommandType, input, current) {
		response.Verdict, response.ReasonCode = "DENY", "SNAPSHOT_MISMATCH"
	}
	if _, err := approvalencoding.EncodeProofResponse(response); err != nil {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	return response, nil
}

func tokenProofSnapshotMatches(command string, frozen approvalencoding.LegacyProofInput, current ApprovalSnapshotProjection) bool {
	if !jsonSemanticEqual(frozen.CriticalResourceVersions, current.CriticalResourceVersions) {
		return false
	}
	if command == "TOKEN_CREDENTIAL_REVOKE" {
		return jsonSemanticEqual(frozen.FinancialSnapshot, current.FinancialSnapshot)
	}
	return approvalpolicy.SameTokenModelFinancialFacts(frozen.FinancialSnapshot, current.FinancialSnapshot)
}
