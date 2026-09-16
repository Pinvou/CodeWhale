package token

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
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
	stopClose := context.AfterFunc(ctx, func() { _ = r.Body.Close() })
	defer stopClose()
	body, err := io.ReadAll(io.LimitReader(r.Body, approvalencoding.MaximumDocumentBytes+1))
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "TOKEN_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
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
	response, err := s.tokenApprovalProof(ctx, request)
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "TOKEN_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_APPROVAL_PROOF_UNAVAILABLE", "authoritative proof cannot be established", httpserver.RequestID(r.Context()))
		return
	}
	encoded, err := approvalencoding.EncodeProofResponse(response)
	if ctx.Err() != nil {
		api.WriteError(w, http.StatusGatewayTimeout, "TOKEN_APPROVAL_PROOF_TIMEOUT", "proof deadline expired", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil || !approvalencoding.ProofTransportWithinBudget(body, encoded) {
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
	if artifacts.CommandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" && !tokenLifecyclePlanExecutable(input.NormalizedCommand) {
		// Frozen historical immediate requests remain decodable for audit, but
		// the owner executor can only schedule an immutable persisted plan.
		response.Verdict, response.ReasonCode = "DENY", "DOMAIN_INVARIANT_FAILED"
	} else if !tokenProofSnapshotMatches(artifacts.CommandType, input, current) {
		response.Verdict, response.ReasonCode = "DENY", "SNAPSHOT_MISMATCH"
	}
	if _, err := approvalencoding.EncodeProofResponse(response); err != nil {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	return response, nil
}

func tokenLifecyclePlanExecutable(command json.RawMessage) bool {
	var plan struct {
		ID      *string `json:"lifecycle_plan_id"`
		Version *int64  `json:"lifecycle_plan_version"`
	}
	if json.Unmarshal(command, &plan) != nil || plan.ID == nil || plan.Version == nil || *plan.Version < 1 {
		return false
	}
	id, err := uuid.Parse(*plan.ID)
	return err == nil && id != uuid.Nil && id.String() == *plan.ID
}

func tokenProofSnapshotMatches(command string, frozen approvalencoding.LegacyProofInput, current ApprovalSnapshotProjection) bool {
	if !equalTokenApprovalJSONObjects(frozen.CriticalResourceVersions, current.CriticalResourceVersions) {
		return false
	}
	if command == "TOKEN_CREDENTIAL_REVOKE" {
		return equalTokenApprovalJSONObjects(frozen.FinancialSnapshot, current.FinancialSnapshot)
	}
	return approvalpolicy.SameTokenModelFinancialFacts(frozen.FinancialSnapshot, current.FinancialSnapshot)
}

// tokenExecutionProof checks the frozen event binding and a fresh domain read
// before the first execution claim. Existing result recovery does not take a
// second proof; downstream command transactions still own their local CAS.
func (s *Service) tokenExecutionProof(ctx context.Context, command string, body approvedTokenCommandBody, activation tokenRouteActivationSelection) error {
	if body.ProofSchemaVersion != approvalencoding.ProofSchemaVersion || !validImpactFingerprint(body.CommandDigest) ||
		!validImpactFingerprint(body.SnapshotDigest) || !validImpactFingerprint(body.ProofDigest) ||
		body.ApprovalControlEpoch < 1 || !activation.transactional || activation.State != "ACTIVE" ||
		body.ActivationEpoch != activation.Epoch {
		return errTokenApprovalProofUnavailable
	}
	parts := strings.Split(body.DomainRevision, ":")
	if len(parts) != 2 || parts[0] != strconv.FormatInt(activation.Epoch, 10) || !validImpactFingerprint(parts[1]) {
		return errTokenApprovalProofUnavailable
	}
	policy, err := approvalpolicy.Lookup(command)
	if err != nil {
		return errTokenApprovalProofUnavailable
	}
	schemaVersion := 1
	if command == "TOKEN_MODEL_LIFECYCLE_CHANGE" || command == "TOKEN_MODEL_CATALOG_ROLLBACK" {
		schemaVersion = 2
	}
	frozen := approvalencoding.LegacyProofInput{
		CommandType: command, PolicyVersion: policy.PolicyVersion, SnapshotSchemaVersion: schemaVersion,
		TargetResourceType: body.Envelope.TargetResourceType, TargetResourceID: body.Envelope.TargetResourceID,
		NormalizedCommand: body.Envelope.Command, CriticalResourceVersions: body.CriticalResourceVersions,
		FinancialSnapshot: body.FinancialSnapshot,
	}
	artifacts, err := approvalencoding.ConvertLegacyProofInput(frozen)
	if err != nil || artifacts.CommandDigest != body.CommandDigest || artifacts.SnapshotDigest != body.SnapshotDigest {
		return errTokenApprovalProofUnavailable
	}
	if command == "TOKEN_MODEL_LIFECYCLE_CHANGE" && !tokenLifecyclePlanExecutable(frozen.NormalizedCommand) {
		return errTokenApprovalProofUnavailable
	}
	current, err := s.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{
		CommandType: command, TargetResourceType: frozen.TargetResourceType, TargetResourceID: frozen.TargetResourceID,
		NormalizedCommand: frozen.NormalizedCommand,
	})
	if err != nil || current.PrecheckFingerprint != parts[1] || !tokenProofSnapshotMatches(command, frozen, current) {
		return errTokenApprovalProofUnavailable
	}
	return nil
}
