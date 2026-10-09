package approvalencoding

import (
	"bytes"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

var legacyEnvelopeKeys = []string{"event_id", "event_type", "aggregate_id", "aggregate_version", "occurred_at", "trace_id", "payload"}
var newEnvelopeKeys = []string{"serialization_version", "event_id", "event_type", "aggregate_id", "aggregate_version", "occurred_at", "trace_id", "payload"}
var authorizationPayloadKeys = []string{"authorization_schema_version", "approval_request_id", "command_type", "command_fingerprint", "policy_version", "executor_identity", "execution_deadline", "normalized_command", "target_resource_type", "target_resource_id", "critical_resource_versions", "financial_snapshot", "proof_schema_version", "command_digest", "snapshot_digest", "domain_revision", "proof_digest", "approval_control_epoch"}
var legacyAuthorizationPayloadKeys = []string{"approval_request_id", "command_type", "command_fingerprint", "policy_version", "executor_identity", "execution_deadline", "normalized_command", "target_resource_type", "target_resource_id", "critical_resource_versions", "financial_snapshot"}

func EncodeAuthorizationEvent(event AuthorizationEvent, expected ExpectedAuthorizationBinding) ([]byte, error) {
	if len(event.NormalizedCommand)+len(event.CriticalResourceVersions)+len(event.FinancialSnapshot) > MaximumDocumentBytes {
		return nil, ErrDocumentTooLarge
	}
	normalized, err := parseDocument(event.NormalizedCommand)
	if err != nil {
		return nil, err
	}
	critical, err := parseDocument(event.CriticalResourceVersions)
	if err != nil {
		return nil, err
	}
	financial, err := parseDocument(event.FinancialSnapshot)
	if err != nil {
		return nil, err
	}
	if err := validateAuthorizationEvent(event, expected, normalized, critical, financial); err != nil {
		return nil, err
	}
	payload := newObject(map[string]*jsonValue{
		"authorization_schema_version": jsonString("1"),
		"approval_request_id":          jsonString(event.ApprovalRequestID.String()),
		"command_type":                 jsonString(event.CommandType),
		"command_fingerprint":          jsonString(event.CommandFingerprint),
		"policy_version":               jsonInteger(event.PolicyVersion),
		"executor_identity":            jsonString(event.ExecutorIdentity),
		"execution_deadline":           jsonString(event.ExecutionDeadline.UTC().Format(time.RFC3339Nano)),
		"normalized_command":           normalized,
		"target_resource_type":         jsonString(event.TargetResourceType),
		"target_resource_id":           jsonString(event.TargetResourceID),
		"critical_resource_versions":   critical,
		"financial_snapshot":           financial,
		"proof_schema_version":         jsonString(event.ProofSchemaVersion),
		"command_digest":               jsonString(event.CommandDigest),
		"snapshot_digest":              jsonString(event.SnapshotDigest),
		"domain_revision":              jsonString(event.DomainRevision),
		"proof_digest":                 jsonString(event.ProofDigest),
		"approval_control_epoch":       jsonString(int64Text(event.ApprovalControlEpoch)),
	})
	envelope := newObject(map[string]*jsonValue{
		"serialization_version": jsonString(ApprovalSerializationVersion),
		"event_id":              jsonString(event.EventID.String()),
		"event_type":            jsonString(AuthorizationEventType),
		"aggregate_id":          jsonString(event.AggregateID.String()),
		"aggregate_version":     jsonString(int64Text(event.AggregateVersion)),
		"occurred_at":           jsonString(event.OccurredAt.UTC().Format(time.RFC3339Nano)),
		"trace_id":              jsonString(event.TraceID),
		"payload":               payload,
	})
	encoded, err := canonicalJSON(envelope)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaximumDocumentBytes {
		return nil, ErrDocumentTooLarge
	}
	return encoded, nil
}

// EncodeLegacyAuthorizationEvent preserves the existing events.Event.Marshal
// contract exactly. It never routes legacy bytes through the new JCS encoder.
func EncodeLegacyAuthorizationEvent(event events.Event) ([]byte, error) {
	if event.Type != AuthorizationEventType {
		return nil, ErrInvalidDocument
	}
	encoded, err := event.Marshal()
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaximumDocumentBytes {
		return nil, ErrDocumentTooLarge
	}
	return encoded, nil
}

func DecodeAuthorizationEvent(raw []byte, expected ExpectedAuthorizationBinding) (DecodedAuthorizationEvent, error) {
	root, err := parseDocument(raw)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	rootObject, err := objectValue(root)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	versionValue, hasVersion := rootObject["serialization_version"]
	if !hasVersion {
		return decodeLegacyAuthorizationEvent(raw, root, expected)
	}
	version, err := stringValue(versionValue)
	if err != nil || version != ApprovalSerializationVersion {
		return DecodedAuthorizationEvent{}, ErrUnsupportedVersion
	}
	object, err := requireObject(root, newEnvelopeKeys...)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	payloadObject, err := requireObject(object["payload"], authorizationPayloadKeys...)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	event, err := decodeNewAuthorizationFields(object, payloadObject)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	if err := validateAuthorizationEvent(event, expected, payloadObject["normalized_command"], payloadObject["critical_resource_versions"], payloadObject["financial_snapshot"]); err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	return DecodedAuthorizationEvent{
		SerializationVersion: ApprovalSerializationVersion,
		EventID:              event.EventID, AggregateID: event.AggregateID, AggregateVersion: event.AggregateVersion,
		OccurredAt: event.OccurredAt, TraceID: event.TraceID, Payload: event,
	}, nil
}

func decodeNewAuthorizationFields(envelope, payload map[string]*jsonValue) (AuthorizationEvent, error) {
	eventID, err := strictUUIDValue(envelope["event_id"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	aggregateID, err := strictUUIDValue(envelope["aggregate_id"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	aggregateVersion, err := decimalString(envelope["aggregate_version"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	occurredAt, err := strictTimeValue(envelope["occurred_at"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	traceID, err := stringValue(envelope["trace_id"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	if eventType, err := stringValue(envelope["event_type"]); err != nil || eventType != AuthorizationEventType {
		return AuthorizationEvent{}, ErrInvalidDocument
	}
	if schema, err := stringValue(payload["authorization_schema_version"]); err != nil || schema != "1" {
		return AuthorizationEvent{}, ErrUnsupportedVersion
	}
	requestID, err := strictUUIDValue(payload["approval_request_id"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	policyVersion, err := integerNumber(payload["policy_version"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	executionDeadline, err := strictTimeValue(payload["execution_deadline"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	approvalEpoch, err := decimalString(payload["approval_control_epoch"])
	if err != nil {
		return AuthorizationEvent{}, err
	}
	values := make(map[string]string)
	for _, field := range []string{"command_type", "command_fingerprint", "executor_identity", "target_resource_type", "target_resource_id", "proof_schema_version", "command_digest", "snapshot_digest", "domain_revision", "proof_digest"} {
		values[field], err = stringValue(payload[field])
		if err != nil {
			return AuthorizationEvent{}, err
		}
	}
	return AuthorizationEvent{
		EventID: eventID, AggregateID: aggregateID, AggregateVersion: aggregateVersion, OccurredAt: occurredAt, TraceID: traceID,
		ApprovalRequestID: requestID, CommandType: values["command_type"], CommandFingerprint: values["command_fingerprint"], PolicyVersion: policyVersion,
		ExecutorIdentity: values["executor_identity"], ExecutionDeadline: executionDeadline,
		NormalizedCommand: rawCanonical(payload["normalized_command"]), TargetResourceType: values["target_resource_type"], TargetResourceID: values["target_resource_id"],
		CriticalResourceVersions: rawCanonical(payload["critical_resource_versions"]), FinancialSnapshot: rawCanonical(payload["financial_snapshot"]),
		ProofSchemaVersion: values["proof_schema_version"], CommandDigest: values["command_digest"], SnapshotDigest: values["snapshot_digest"],
		DomainRevision: values["domain_revision"], ProofDigest: values["proof_digest"], ApprovalControlEpoch: approvalEpoch,
	}, nil
}

func decodeLegacyAuthorizationEvent(raw []byte, parsed *jsonValue, expected ExpectedAuthorizationBinding) (DecodedAuthorizationEvent, error) {
	if _, err := requireObject(parsed, legacyEnvelopeKeys...); err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	event, err := events.Unmarshal(raw)
	// LEGACY_TYPED_V1 was produced only by the legacy epoch-1 writer. Later
	// epochs require the in-band proof-era serialization discriminator.
	if err != nil || event.Type != AuthorizationEventType || event.AggregateVersion != expected.ApprovedRequestVersion || expected.ApprovalEpoch != 1 {
		return DecodedAuthorizationEvent{}, ErrBindingMismatch
	}
	payload, err := parseDocument(event.Payload)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	object, err := requireObject(payload, legacyAuthorizationPayloadKeys...)
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	requestID, err := strictUUIDValue(object["approval_request_id"])
	if err != nil || requestID != event.AggregateID {
		return DecodedAuthorizationEvent{}, ErrInvalidDocument
	}
	policyVersion, err := integerNumber(object["policy_version"])
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	commandType, err := stringValue(object["command_type"])
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	targetType, err := stringValue(object["target_resource_type"])
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	targetID, err := stringValue(object["target_resource_id"])
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	spec, ok := commandSpecs[commandType]
	if !ok || policyVersion != spec.policyVersion {
		return DecodedAuthorizationEvent{}, ErrUnsupportedVersion
	}
	artifacts, err := ConvertLegacyProofInput(LegacyProofInput{
		CommandType: commandType, PolicyVersion: policyVersion, SnapshotSchemaVersion: spec.snapshotVersion,
		TargetResourceType: targetType, TargetResourceID: targetID,
		NormalizedCommand: rawCanonical(object["normalized_command"]), CriticalResourceVersions: rawCanonical(object["critical_resource_versions"]), FinancialSnapshot: rawCanonical(object["financial_snapshot"]),
	})
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	fingerprint, err := stringValue(object["command_fingerprint"])
	if err != nil || !hexString(fingerprint) {
		return DecodedAuthorizationEvent{}, ErrInvalidDocument
	}
	executor, err := stringValue(object["executor_identity"])
	if err != nil || executor != spec.executor {
		return DecodedAuthorizationEvent{}, ErrInvalidDocument
	}
	deadline, err := strictTimeValue(object["execution_deadline"])
	if err != nil {
		return DecodedAuthorizationEvent{}, err
	}
	decoded := AuthorizationEvent{
		EventID: event.ID, AggregateID: event.AggregateID, AggregateVersion: event.AggregateVersion, OccurredAt: event.OccurredAt, TraceID: event.TraceID,
		ApprovalRequestID: requestID, CommandType: commandType, CommandFingerprint: fingerprint, PolicyVersion: policyVersion, ExecutorIdentity: executor, ExecutionDeadline: deadline,
		NormalizedCommand: rawCanonical(object["normalized_command"]), TargetResourceType: targetType, TargetResourceID: targetID,
		CriticalResourceVersions: rawCanonical(object["critical_resource_versions"]), FinancialSnapshot: rawCanonical(object["financial_snapshot"]),
		CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest,
	}
	return DecodedAuthorizationEvent{
		SerializationVersion: LegacySerializationVersion, EventID: event.ID, AggregateID: event.AggregateID, AggregateVersion: event.AggregateVersion,
		OccurredAt: event.OccurredAt, TraceID: event.TraceID, Payload: decoded, LegacyPayload: bytes.Clone(event.Payload),
	}, nil
}

func validateAuthorizationEvent(event AuthorizationEvent, expected ExpectedAuthorizationBinding, normalized, critical, financial *jsonValue) error {
	if expected.ApprovedRequestVersion < 1 || expected.ApprovalEpoch < 1 || event.AggregateVersion != expected.ApprovedRequestVersion || event.ApprovalControlEpoch != expected.ApprovalEpoch {
		return ErrBindingMismatch
	}
	if event.EventID == uuid.Nil || event.AggregateID == uuid.Nil || event.ApprovalRequestID == uuid.Nil || event.AggregateID != event.ApprovalRequestID || !canonicalUTC(event.OccurredAt) || !canonicalUTC(event.ExecutionDeadline) || !utf8.ValidString(event.TraceID) || strings.TrimSpace(event.TraceID) == "" {
		return ErrInvalidDocument
	}
	spec, ok := commandSpecs[event.CommandType]
	if !ok || event.PolicyVersion != spec.policyVersion || event.ExecutorIdentity != spec.executor || event.ProofSchemaVersion != ProofSchemaVersion || !hexString(event.CommandFingerprint) || !hexString(event.CommandDigest) || !hexString(event.SnapshotDigest) || !hexString(event.ProofDigest) || !boundedString(event.DomainRevision, 128) {
		return ErrInvalidDocument
	}
	artifacts, err := buildProofArtifacts(event.CommandType, event.PolicyVersion, spec.snapshotVersion, event.TargetResourceType, event.TargetResourceID, normalized, critical, financial)
	if err != nil || artifacts.CommandDigest != event.CommandDigest || artifacts.SnapshotDigest != event.SnapshotDigest {
		return ErrInvalidDocument
	}
	return nil
}

func strictUUIDValue(value *jsonValue) (uuid.UUID, error) {
	text, err := stringValue(value)
	if err != nil || !validUUID(text) {
		return uuid.Nil, ErrInvalidDocument
	}
	return uuid.Parse(text)
}

func strictTimeValue(value *jsonValue) (time.Time, error) {
	text, err := stringValue(value)
	if err != nil || !canonicalTimeString(text) {
		return time.Time{}, ErrInvalidDocument
	}
	return time.Parse(time.RFC3339Nano, text)
}

func int64Text(value int64) string {
	return strconv.FormatInt(value, 10)
}
