package token

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
)

var errTokenApprovalProofUnavailable = errors.New("authoritative Token proof unavailable")

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
	if fresh.SnapshotDigest != artifacts.SnapshotDigest {
		response.Verdict, response.ReasonCode = "DENY", "SNAPSHOT_MISMATCH"
	}
	if _, err := approvalencoding.EncodeProofResponse(response); err != nil {
		return approvalencoding.ProofResponse{}, errTokenApprovalProofUnavailable
	}
	return response, nil
}
