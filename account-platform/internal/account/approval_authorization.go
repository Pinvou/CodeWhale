package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const approvalAuthorizationEventType = "account.approval-command.authorized.v1"

var (
	ErrApprovalAuthorizationControlChanged = errors.New("approval authorization control changed")
	ErrApprovalAuthorizationInvalid        = errors.New("approval authorization facts are invalid")
)

type ApprovalAuthorizationRecord struct {
	EventID              uuid.UUID
	ApprovalRequestID    uuid.UUID
	AggregateVersion     int64
	ApprovalControlEpoch int64
	SerializationVersion string
	Provenance           string
	DeliveryBytes        []byte
	EventDigest          string
}

type persistedProofAuthorizationPayload struct {
	AuthorizationSchemaVersion string          `json:"authorization_schema_version"`
	ApprovalRequestID          string          `json:"approval_request_id"`
	CommandType                string          `json:"command_type"`
	CommandFingerprint         string          `json:"command_fingerprint"`
	PolicyVersion              json.Number     `json:"policy_version"`
	ExecutorIdentity           string          `json:"executor_identity"`
	ExecutionDeadline          string          `json:"execution_deadline"`
	NormalizedCommand          json.RawMessage `json:"normalized_command"`
	TargetResourceType         string          `json:"target_resource_type"`
	TargetResourceID           string          `json:"target_resource_id"`
	CriticalResourceVersions   json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot          json.RawMessage `json:"financial_snapshot"`
	ProofSchemaVersion         string          `json:"proof_schema_version"`
	CommandDigest              string          `json:"command_digest"`
	SnapshotDigest             string          `json:"snapshot_digest"`
	DomainRevision             string          `json:"domain_revision"`
	ProofDigest                string          `json:"proof_digest"`
	ApprovalControlEpoch       string          `json:"approval_control_epoch"`
}

type ApprovalAuthorizationProof struct {
	Artifacts approvalencoding.ProofArtifacts
	Response  approvalencoding.ProofResponse
}

func lockApprovalAuthorizationControl(ctx context.Context, tx pgx.Tx) (AdminApprovalControl, error) {
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return AdminApprovalControl{}, err
	}
	actions, err := AdminApprovalActionsFor(control)
	if err != nil {
		return AdminApprovalControl{}, err
	}
	if !actions.Approve || control.AuthorizationRelayState != AdminApprovalRelayRunning {
		return AdminApprovalControl{}, ErrApprovalAuthorizationControlChanged
	}
	return control, nil
}

// appendLegacyApprovalAuthorization is the one-transaction legacy writer
// seam. The caller owns tx and must have already moved the request to APPROVED;
// this function re-locks all authoritative facts before calling the only
// database function allowed to create an authorization Outbox row + sidecar.
func appendLegacyApprovalAuthorization(
	ctx context.Context,
	tx pgx.Tx,
	expectedControl AdminApprovalControl,
	requestID uuid.UUID,
	eventID uuid.UUID,
) (ApprovalAuthorizationRecord, error) {
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if control != expectedControl || control.Mode != AdminApprovalModeLegacy || control.AuthorizationRelayState != AdminApprovalRelayRunning || control.ApprovalEpoch != 1 {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationControlChanged
	}
	// The caller's status CAS already holds the request row lock. Re-read the
	// complete snapshot without requesting broader table-level UPDATE rights.
	request, err := approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if request.Status != "APPROVED" || request.ExecutionDeadline == nil || request.Version < 1 || eventID == uuid.Nil {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	policy, err := approvalPolicyForItem(request)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	executionDeadline := request.ExecutionDeadline.UTC()
	payload, err := json.Marshal(map[string]any{
		"approval_request_id":        request.ID,
		"command_type":               request.CommandType,
		"command_fingerprint":        request.CommandFingerprint,
		"policy_version":             request.PolicyVersion,
		"executor_identity":          policy.ExecutorIdentity,
		"execution_deadline":         executionDeadline,
		"normalized_command":         request.NormalizedCommand,
		"target_resource_type":       request.TargetResourceType,
		"target_resource_id":         request.TargetResourceID,
		"critical_resource_versions": request.CriticalResourceVersions,
		"financial_snapshot":         request.FinancialSnapshot,
	})
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	traceID := observability.TraceID(ctx)
	if traceID == "" {
		traceID = observability.RequestID(ctx)
	}
	if traceID == "" {
		traceID = "internal"
	}
	var persisted struct {
		EventID              uuid.UUID
		EventType            string
		AggregateID          uuid.UUID
		AggregateVersion     int64
		Payload              json.RawMessage
		TraceID              string
		OccurredAt           time.Time
		ApprovalControlEpoch int64
	}
	if err := tx.QueryRow(ctx, `SELECT * FROM account_approval_authorization.prepare_authorization_event($1,$2,$3,$4,$5,$6,$7)`,
		eventID, request.ID, request.Version, payload, traceID, control.ApprovalEpoch,
		approvalencoding.LegacySerializationVersion,
	).Scan(&persisted.EventID, &persisted.EventType, &persisted.AggregateID, &persisted.AggregateVersion,
		&persisted.Payload, &persisted.TraceID, &persisted.OccurredAt, &persisted.ApprovalControlEpoch); err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	deliveryBytes, err := approvalencoding.EncodeLegacyAuthorizationEvent(events.Event{
		ID: persisted.EventID, Type: persisted.EventType, AggregateID: persisted.AggregateID,
		AggregateVersion: persisted.AggregateVersion, Payload: persisted.Payload,
		TraceID: persisted.TraceID, OccurredAt: persisted.OccurredAt.UTC(),
	})
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if _, err := approvalencoding.DecodeAuthorizationEvent(deliveryBytes, approvalencoding.ExpectedAuthorizationBinding{
		ApprovedRequestVersion: request.Version,
		ApprovalEpoch:          control.ApprovalEpoch,
	}); err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT account_approval_authorization.finalize_authorization_event($1,$2,$3)`,
		eventID, approvalencoding.LegacySerializationVersion, deliveryBytes,
	); err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	digest := sha256.Sum256(deliveryBytes)
	return ApprovalAuthorizationRecord{
		EventID: eventID, ApprovalRequestID: request.ID, AggregateVersion: request.Version,
		ApprovalControlEpoch: persisted.ApprovalControlEpoch, SerializationVersion: approvalencoding.LegacySerializationVersion,
		Provenance: "FROZEN_AT_APPROVAL", DeliveryBytes: deliveryBytes, EventDigest: hex.EncodeToString(digest[:]),
	}, nil
}

// appendProofApprovalAuthorization is the proof-era writer seam required by
// the frozen wire contract. GSA-06 deliberately does not call it from the live
// APPROVE path; GSA-13 may do so only after proof verification and activation.
func appendProofApprovalAuthorization(
	ctx context.Context,
	tx pgx.Tx,
	expectedControl AdminApprovalControl,
	requestID uuid.UUID,
	eventID uuid.UUID,
	proof ApprovalAuthorizationProof,
) (ApprovalAuthorizationRecord, error) {
	control, err := LockAdminApprovalControlShared(ctx, tx)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if control != expectedControl || control.Mode != AdminApprovalModeProofRequired || control.AuthorizationRelayState != AdminApprovalRelayRunning || !control.ProofRequired || control.ApprovalWireVersion != 2 {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationControlChanged
	}
	request, err := approvalRequestByID(ctx, tx, requestID)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if request.Status != "APPROVED" || request.ExecutionDeadline == nil || request.Version < 1 || eventID == uuid.Nil {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	policy, err := approvalPolicyForItem(request)
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	executionDeadline := request.ExecutionDeadline.UTC()
	expectedArtifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: request.CommandType, PolicyVersion: request.PolicyVersion,
		SnapshotSchemaVersion: request.SnapshotSchemaVersion,
		TargetResourceType:    request.TargetResourceType, TargetResourceID: request.TargetResourceID,
		NormalizedCommand: request.NormalizedCommand, CriticalResourceVersions: request.CriticalResourceVersions,
		FinancialSnapshot: request.FinancialSnapshot,
	})
	if err != nil || !sameProofArtifacts(expectedArtifacts, proof.Artifacts) {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	verifiedArtifacts, err := approvalencoding.DecodeProofInput(request.CommandType, request.PolicyVersion, proof.Artifacts.ProofInputJSON)
	if err != nil || !sameProofArtifacts(expectedArtifacts, verifiedArtifacts) {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	response := proof.Response
	if response.ApprovalRequestID != request.ID || response.CommandType != request.CommandType ||
		response.PolicyVersion != request.PolicyVersion || response.ProofSchemaVersion != approvalencoding.ProofSchemaVersion ||
		response.CommandDigest != proof.Artifacts.CommandDigest || response.SnapshotDigest != proof.Artifacts.SnapshotDigest ||
		response.Verdict != "ALLOW" {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	proofDigest, err := approvalencoding.DeriveProofDigest(response)
	if err != nil {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	proofDigestText := hex.EncodeToString(proofDigest[:])
	var proofInput struct {
		NormalizedCommand        json.RawMessage `json:"normalized_command"`
		CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
		FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	}
	if err := json.Unmarshal(proof.Artifacts.ProofInputJSON, &proofInput); err != nil {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	payload, err := json.Marshal(map[string]any{
		"authorization_schema_version": "1", "approval_request_id": request.ID,
		"command_type": request.CommandType, "command_fingerprint": request.CommandFingerprint,
		"policy_version": request.PolicyVersion, "executor_identity": policy.ExecutorIdentity,
		"execution_deadline": executionDeadline, "normalized_command": proofInput.NormalizedCommand,
		"target_resource_type": request.TargetResourceType, "target_resource_id": request.TargetResourceID,
		"critical_resource_versions": proofInput.CriticalResourceVersions, "financial_snapshot": proofInput.FinancialSnapshot,
		"proof_schema_version": approvalencoding.ProofSchemaVersion,
		"command_digest":       proof.Artifacts.CommandDigest, "snapshot_digest": proof.Artifacts.SnapshotDigest,
		"domain_revision": response.DomainRevision, "proof_digest": proofDigestText,
		"approval_control_epoch": fmt.Sprintf("%d", control.ApprovalEpoch),
	})
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	traceID := observability.TraceID(ctx)
	if traceID == "" {
		traceID = observability.RequestID(ctx)
	}
	if traceID == "" {
		traceID = "internal"
	}
	var persisted struct {
		EventID              uuid.UUID
		EventType            string
		AggregateID          uuid.UUID
		AggregateVersion     int64
		Payload              json.RawMessage
		TraceID              string
		OccurredAt           time.Time
		ApprovalControlEpoch int64
	}
	if err := tx.QueryRow(ctx, `SELECT * FROM account_approval_authorization.prepare_authorization_event($1,$2,$3,$4,$5,$6,$7)`,
		eventID, request.ID, request.Version, payload, traceID, control.ApprovalEpoch,
		approvalencoding.ApprovalSerializationVersion,
	).Scan(&persisted.EventID, &persisted.EventType, &persisted.AggregateID, &persisted.AggregateVersion,
		&persisted.Payload, &persisted.TraceID, &persisted.OccurredAt, &persisted.ApprovalControlEpoch); err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	persistedEvent, err := decodePersistedProofAuthorizationPayload(persisted.Payload)
	if err != nil {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	persistedEvent.EventID = persisted.EventID
	persistedEvent.AggregateID = persisted.AggregateID
	persistedEvent.AggregateVersion = persisted.AggregateVersion
	persistedEvent.OccurredAt = persisted.OccurredAt.UTC()
	persistedEvent.TraceID = persisted.TraceID
	if persisted.EventType != approvalencoding.AuthorizationEventType || persistedEvent.ApprovalRequestID != request.ID ||
		persistedEvent.CommandType != request.CommandType || persistedEvent.CommandFingerprint != request.CommandFingerprint ||
		persistedEvent.PolicyVersion != request.PolicyVersion || persistedEvent.ExecutorIdentity != policy.ExecutorIdentity ||
		!persistedEvent.ExecutionDeadline.Equal(executionDeadline) || persistedEvent.TargetResourceType != request.TargetResourceType ||
		persistedEvent.TargetResourceID != request.TargetResourceID || persistedEvent.ProofSchemaVersion != approvalencoding.ProofSchemaVersion ||
		persistedEvent.CommandDigest != proof.Artifacts.CommandDigest || persistedEvent.SnapshotDigest != proof.Artifacts.SnapshotDigest ||
		persistedEvent.DomainRevision != response.DomainRevision || persistedEvent.ProofDigest != proofDigestText ||
		persistedEvent.ApprovalControlEpoch != control.ApprovalEpoch || persisted.ApprovalControlEpoch != control.ApprovalEpoch {
		return ApprovalAuthorizationRecord{}, ErrApprovalAuthorizationInvalid
	}
	deliveryBytes, err := approvalencoding.EncodeAuthorizationEvent(persistedEvent,
		approvalencoding.ExpectedAuthorizationBinding{ApprovedRequestVersion: request.Version, ApprovalEpoch: control.ApprovalEpoch})
	if err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT account_approval_authorization.finalize_authorization_event($1,$2,$3)`,
		eventID, approvalencoding.ApprovalSerializationVersion, deliveryBytes); err != nil {
		return ApprovalAuthorizationRecord{}, err
	}
	digest := sha256.Sum256(deliveryBytes)
	return ApprovalAuthorizationRecord{
		EventID: eventID, ApprovalRequestID: request.ID, AggregateVersion: request.Version,
		ApprovalControlEpoch: persisted.ApprovalControlEpoch, SerializationVersion: approvalencoding.ApprovalSerializationVersion,
		Provenance: "FROZEN_AT_APPROVAL", DeliveryBytes: deliveryBytes, EventDigest: hex.EncodeToString(digest[:]),
	}, nil
}

func decodePersistedProofAuthorizationPayload(raw json.RawMessage) (approvalencoding.AuthorizationEvent, error) {
	if len(raw) == 0 || len(raw) > approvalencoding.MaximumDocumentBytes {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var payload persistedProofAuthorizationPayload
	if err := decoder.Decode(&payload); err != nil {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	if payload.AuthorizationSchemaVersion != "1" {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	requestID, err := uuid.Parse(payload.ApprovalRequestID)
	if err != nil || requestID == uuid.Nil || requestID.String() != payload.ApprovalRequestID {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	policyVersion, err := strconv.ParseInt(payload.PolicyVersion.String(), 10, 64)
	if err != nil || strconv.FormatInt(policyVersion, 10) != payload.PolicyVersion.String() {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	executionDeadline, err := time.Parse(time.RFC3339Nano, payload.ExecutionDeadline)
	if err != nil || executionDeadline.IsZero() || executionDeadline.Location() != time.UTC || executionDeadline.Format(time.RFC3339Nano) != payload.ExecutionDeadline {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	approvalEpoch, err := strconv.ParseInt(payload.ApprovalControlEpoch, 10, 64)
	if err != nil || approvalEpoch < 1 || strconv.FormatInt(approvalEpoch, 10) != payload.ApprovalControlEpoch {
		return approvalencoding.AuthorizationEvent{}, ErrApprovalAuthorizationInvalid
	}
	return approvalencoding.AuthorizationEvent{
		ApprovalRequestID: requestID, CommandType: payload.CommandType, CommandFingerprint: payload.CommandFingerprint,
		PolicyVersion: policyVersion, ExecutorIdentity: payload.ExecutorIdentity, ExecutionDeadline: executionDeadline,
		NormalizedCommand: payload.NormalizedCommand, TargetResourceType: payload.TargetResourceType, TargetResourceID: payload.TargetResourceID,
		CriticalResourceVersions: payload.CriticalResourceVersions, FinancialSnapshot: payload.FinancialSnapshot,
		ProofSchemaVersion: payload.ProofSchemaVersion, CommandDigest: payload.CommandDigest, SnapshotDigest: payload.SnapshotDigest,
		DomainRevision: payload.DomainRevision, ProofDigest: payload.ProofDigest, ApprovalControlEpoch: approvalEpoch,
	}, nil
}

func sameProofArtifacts(left, right approvalencoding.ProofArtifacts) bool {
	return left.CommandType == right.CommandType && left.PolicyVersion == right.PolicyVersion &&
		left.SnapshotSchemaVersion == right.SnapshotSchemaVersion &&
		bytes.Equal(left.ProofInputJSON, right.ProofInputJSON) && bytes.Equal(left.CommandJCS, right.CommandJCS) &&
		bytes.Equal(left.SnapshotJCS, right.SnapshotJCS) && left.CommandDigest == right.CommandDigest &&
		left.SnapshotDigest == right.SnapshotDigest
}
