package approvalencoding

import (
	"bytes"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

type ProofRequest struct {
	ApprovalRequestID uuid.UUID
	Artifacts         ProofArtifacts
}

// DecodeProofRequest is the owner-domain counterpart to EncodeProofRequest.
// It checks the exact seven transport fields, the registered command/policy
// variant, typed closed proof input and both locally recomputed digests.
func DecodeProofRequest(raw []byte) (ProofRequest, error) {
	value, err := parseDocument(raw)
	if err != nil {
		return ProofRequest{}, err
	}
	fields, err := requireObject(value, "approval_request_id", "command_type", "policy_version", "proof_schema_version", "command_digest", "snapshot_digest", "proof_input")
	if err != nil {
		return ProofRequest{}, err
	}
	id, err := stringValue(fields["approval_request_id"])
	if err != nil || !validUUID(id) {
		return ProofRequest{}, ErrInvalidDocument
	}
	command, err := stringValue(fields["command_type"])
	if err != nil {
		return ProofRequest{}, err
	}
	policyVersion, err := integerNumber(fields["policy_version"])
	if err != nil {
		return ProofRequest{}, err
	}
	schema, err := stringValue(fields["proof_schema_version"])
	if err != nil || schema != ProofSchemaVersion {
		return ProofRequest{}, ErrUnsupportedVersion
	}
	input, err := canonicalJSON(fields["proof_input"])
	if err != nil {
		return ProofRequest{}, err
	}
	artifacts, err := DecodeProofInput(command, policyVersion, input)
	if err != nil {
		return ProofRequest{}, ErrInvalidDocument
	}
	commandDigest, err := stringValue(fields["command_digest"])
	if err != nil || commandDigest != artifacts.CommandDigest {
		return ProofRequest{}, ErrInvalidDocument
	}
	snapshotDigest, err := stringValue(fields["snapshot_digest"])
	if err != nil || snapshotDigest != artifacts.SnapshotDigest {
		return ProofRequest{}, ErrInvalidDocument
	}
	return ProofRequest{ApprovalRequestID: uuid.MustParse(id), Artifacts: artifacts}, nil
}

// EncodeProofRequest validates the typed artifacts again at the transport
// boundary. No arbitrary browser or presenter JSON can be inserted here.
func EncodeProofRequest(requestID uuid.UUID, artifacts ProofArtifacts) ([]byte, error) {
	if requestID == uuid.Nil || !validUUID(requestID.String()) || artifacts.CommandType == "" {
		return nil, ErrInvalidDocument
	}
	verified, err := DecodeProofInput(artifacts.CommandType, artifacts.PolicyVersion, artifacts.ProofInputJSON)
	if err != nil || verified.SnapshotSchemaVersion != artifacts.SnapshotSchemaVersion ||
		verified.CommandDigest != artifacts.CommandDigest || verified.SnapshotDigest != artifacts.SnapshotDigest ||
		!bytes.Equal(verified.ProofInputJSON, artifacts.ProofInputJSON) {
		return nil, ErrInvalidDocument
	}
	proofInput, err := parseDocument(verified.ProofInputJSON)
	if err != nil {
		return nil, err
	}
	request := newObject(map[string]*jsonValue{
		"approval_request_id":  jsonString(requestID.String()),
		"command_type":         jsonString(artifacts.CommandType),
		"command_digest":       jsonString(artifacts.CommandDigest),
		"snapshot_digest":      jsonString(artifacts.SnapshotDigest),
		"policy_version":       jsonInteger(artifacts.PolicyVersion),
		"proof_schema_version": jsonString(ProofSchemaVersion),
		"proof_input":          proofInput,
	})
	encoded, err := canonicalJSON(request)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaximumDocumentBytes {
		return nil, ErrDocumentTooLarge
	}
	return encoded, nil
}

// EncodeProofResponse emits exactly the eleven approved transport fields.
// Validation is shared with the consumer's digest derivation so the owner
// cannot accidentally publish an unbound or non-canonical proof.
func EncodeProofResponse(response ProofResponse) ([]byte, error) {
	if _, err := DeriveProofDigest(response); err != nil {
		return nil, err
	}
	value := newObject(map[string]*jsonValue{
		"approval_request_id":  jsonString(response.ApprovalRequestID.String()),
		"command_type":         jsonString(response.CommandType),
		"command_digest":       jsonString(response.CommandDigest),
		"snapshot_digest":      jsonString(response.SnapshotDigest),
		"policy_version":       jsonInteger(response.PolicyVersion),
		"proof_schema_version": jsonString(response.ProofSchemaVersion),
		"domain_revision":      jsonString(response.DomainRevision),
		"verdict":              jsonString(response.Verdict),
		"reason_code":          jsonString(response.ReasonCode),
		"issued_at":            jsonString(response.IssuedAt.Format(time.RFC3339Nano)),
		"expires_at":           jsonString(response.ExpiresAt.Format(time.RFC3339Nano)),
	})
	encoded, err := canonicalJSON(value)
	if err != nil || len(encoded) > MaximumDocumentBytes {
		return nil, ErrDocumentTooLarge
	}
	return encoded, nil
}

// DecodeProofResponse rejects unknown, duplicate and trailing fields before
// constructing a typed value. It only validates the shared transport; the
// caller must bind request/digests, time budget and command-specific reasons.
func DecodeProofResponse(raw []byte) (ProofResponse, string, error) {
	value, err := parseDocument(raw)
	if err != nil {
		return ProofResponse{}, "", err
	}
	fields, err := requireObject(value, "approval_request_id", "command_type", "command_digest", "snapshot_digest",
		"policy_version", "proof_schema_version", "domain_revision", "verdict", "reason_code", "issued_at", "expires_at")
	if err != nil {
		return ProofResponse{}, "", err
	}
	requestID, err := stringValue(fields["approval_request_id"])
	if err != nil || !validUUID(requestID) {
		return ProofResponse{}, "", ErrInvalidDocument
	}
	response := ProofResponse{ApprovalRequestID: uuid.MustParse(requestID)}
	var ok bool
	if response.CommandType, err = stringValue(fields["command_type"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.CommandDigest, err = stringValue(fields["command_digest"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.SnapshotDigest, err = stringValue(fields["snapshot_digest"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.PolicyVersion, err = integerNumber(fields["policy_version"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.ProofSchemaVersion, err = stringValue(fields["proof_schema_version"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.DomainRevision, err = stringValue(fields["domain_revision"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.Verdict, err = stringValue(fields["verdict"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.ReasonCode, err = stringValue(fields["reason_code"]); err != nil {
		return ProofResponse{}, "", err
	}
	if response.IssuedAt, ok = requiredTime(fields["issued_at"]); !ok {
		return ProofResponse{}, "", ErrInvalidDocument
	}
	if response.ExpiresAt, ok = requiredTime(fields["expires_at"]); !ok {
		return ProofResponse{}, "", ErrInvalidDocument
	}
	digest, err := DeriveProofDigest(response)
	if err != nil {
		return ProofResponse{}, "", err
	}
	return response, hex.EncodeToString(digest[:]), nil
}

// ProofTransportWithinBudget enforces the aggregate request+response limit,
// independent of the single-document bound in parseDocument.
func ProofTransportWithinBudget(request, response []byte) bool {
	return len(request) <= MaximumDocumentBytes && len(response) <= MaximumDocumentBytes &&
		len(request) <= MaximumDocumentBytes-len(response)
}

// ProofResponseFresh applies the frozen 30-second clock-skew and exact 60s
// TTL. The caller still checks expiration in the final Account transaction.
func ProofResponseFresh(response ProofResponse, now time.Time) bool {
	if now.IsZero() || !canonicalUTC(response.IssuedAt) || !canonicalUTC(response.ExpiresAt) {
		return false
	}
	if response.IssuedAt.Before(now.Add(-30*time.Second)) || response.IssuedAt.After(now.Add(30*time.Second)) {
		return false
	}
	return response.ExpiresAt.Equal(response.IssuedAt.Add(time.Minute)) && response.ExpiresAt.After(now)
}

func ProofResponseBound(response ProofResponse, requestID uuid.UUID, artifacts ProofArtifacts) bool {
	return response.ApprovalRequestID == requestID && response.CommandType == artifacts.CommandType &&
		response.PolicyVersion == artifacts.PolicyVersion && response.ProofSchemaVersion == ProofSchemaVersion &&
		response.CommandDigest == artifacts.CommandDigest && response.SnapshotDigest == artifacts.SnapshotDigest
}
