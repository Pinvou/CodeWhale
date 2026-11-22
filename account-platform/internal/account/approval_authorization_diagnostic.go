package account

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// ApprovalAuthorizationBackfillDiagnostic never retains the original SQL
// error or payload. Report is the only operator-visible representation.
type approvalAuthorizationBackfillDiagnostic struct {
	eventID       uuid.UUID
	serialization string
	digest        string
	reason        string
}

type ApprovalAuthorizationBlockerReport struct {
	EventID              uuid.UUID `json:"event_id"`
	Reason               string    `json:"reason"`
	SchemaVersion        int       `json:"schema_version"`
	SerializationVersion string    `json:"serialization_version"`
	PayloadDigestPrefix  string    `json:"payload_digest_prefix"`
}

func (e *approvalAuthorizationBackfillDiagnostic) Error() string {
	return "approval authorization row is blocked"
}
func (e *approvalAuthorizationBackfillDiagnostic) Unwrap() error {
	return ErrApprovalBackfillUnavailable
}
func (e *approvalAuthorizationBackfillDiagnostic) report() ApprovalAuthorizationBlockerReport {
	return ApprovalAuthorizationBlockerReport{EventID: e.eventID, Reason: e.reason, SchemaVersion: 1, SerializationVersion: e.serialization, PayloadDigestPrefix: e.digest}
}

func newApprovalAuthorizationBackfillDiagnostic(candidate approvalAuthorizationBackfillCandidate, reason string) error {
	switch reason {
	case "INVALID_EVENT_IDENTITY", "LEGACY_RECONSTRUCTION_FAILED", "LEGACY_CLOSED_DECODE_FAILED",
		"MISSING_IMMUTABLE_EVIDENCE", "SIDECAR_BINDING_MISMATCH", "DELIVERY_EVIDENCE_MISMATCH",
		"SEMANTIC_EVIDENCE_MISMATCH", "UNKNOWN_PROVENANCE", "ROW_WRITE_REJECTED", "ROW_WRITE_OUTCOME_UNKNOWN":
	default:
		reason = "UNKNOWN"
	}
	serialization := approvalencoding.LegacySerializationVersion
	if candidate.SidecarSerializationVersion != nil {
		switch *candidate.SidecarSerializationVersion {
		case approvalencoding.LegacySerializationVersion, approvalencoding.ApprovalSerializationVersion:
			serialization = *candidate.SidecarSerializationVersion
		default:
			serialization = "UNKNOWN"
		}
	}
	digest := sha256.Sum256(candidate.Payload)
	return &approvalAuthorizationBackfillDiagnostic{eventID: candidate.EventID, serialization: serialization, digest: hex.EncodeToString(digest[:6]), reason: reason}
}

// ApprovalAuthorizationBlockerReportFrom exposes only verified local reports.
// Foreign errors and invalid zero values must not cross the CLI boundary.
func ApprovalAuthorizationBlockerReportFrom(err error) (ApprovalAuthorizationBlockerReport, bool) {
	var diagnostic *approvalAuthorizationBackfillDiagnostic
	if !errors.As(err, &diagnostic) || diagnostic == nil || diagnostic.eventID == uuid.Nil || diagnostic.reason == "" || diagnostic.serialization == "" || len(diagnostic.digest) != 12 {
		return ApprovalAuthorizationBlockerReport{}, false
	}
	return diagnostic.report(), true
}
