package approvalencoding

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const (
	ProofSchemaVersion           = "1"
	LegacySerializationVersion   = "LEGACY_TYPED_V1"
	ApprovalSerializationVersion = "APPROVAL_RFC8785_DECIMAL_V1"
	AuthorizationEventType       = "account.approval-command.authorized.v1"
	MaximumDocumentBytes         = 64 * 1024
	maximumJSONDepth             = 32
)

var (
	ErrInvalidDocument    = errors.New("invalid approval encoding document")
	ErrDocumentTooLarge   = errors.New("approval encoding document too large")
	ErrUnsupportedCommand = errors.New("unsupported approval command")
	ErrUnsupportedVersion = errors.New("unsupported approval encoding version")
	ErrBindingMismatch    = errors.New("approval authorization binding mismatch")
)

// LegacyProofInput is the exact Account v1 storage projection. Integer fields
// inside the three JSON objects are JSON numbers and are converted losslessly
// to the proof-schema decimal-string representation.
type LegacyProofInput struct {
	CommandType              string
	PolicyVersion            int64
	SnapshotSchemaVersion    int
	TargetResourceType       string
	TargetResourceID         string
	NormalizedCommand        json.RawMessage
	CriticalResourceVersions json.RawMessage
	FinancialSnapshot        json.RawMessage
}

// ProofArtifacts contains canonical proof-schema bytes. Callers must treat all
// byte slices as immutable.
type ProofArtifacts struct {
	CommandType           string
	PolicyVersion         int64
	SnapshotSchemaVersion int
	ProofInputJSON        []byte
	CommandJCS            []byte
	SnapshotJCS           []byte
	CommandDigest         string
	SnapshotDigest        string
}

// ProofResponse is the closed eleven-field domain response. ProofDigest is
// deliberately absent: Account derives it locally after validating this value.
type ProofResponse struct {
	ApprovalRequestID  uuid.UUID
	CommandType        string
	CommandDigest      string
	SnapshotDigest     string
	PolicyVersion      int64
	ProofSchemaVersion string
	DomainRevision     string
	Verdict            string
	ReasonCode         string
	IssuedAt           time.Time
	ExpiresAt          time.Time
}

// ExpectedAuthorizationBinding is supplied from caller-owned authoritative
// state. This package does not read Account, Outbox, control, or sidecar data.
type ExpectedAuthorizationBinding struct {
	ApprovedRequestVersion int64
	ApprovalEpoch          int64
}

// AuthorizationEvent is the proof-era authorization wire projection.
type AuthorizationEvent struct {
	EventID                  uuid.UUID
	AggregateID              uuid.UUID
	AggregateVersion         int64
	OccurredAt               time.Time
	TraceID                  string
	ApprovalRequestID        uuid.UUID
	CommandType              string
	CommandFingerprint       string
	PolicyVersion            int64
	ExecutorIdentity         string
	ExecutionDeadline        time.Time
	NormalizedCommand        json.RawMessage
	TargetResourceType       string
	TargetResourceID         string
	CriticalResourceVersions json.RawMessage
	FinancialSnapshot        json.RawMessage
	ProofSchemaVersion       string
	CommandDigest            string
	SnapshotDigest           string
	DomainRevision           string
	ProofDigest              string
	ApprovalControlEpoch     int64
}

// DecodedAuthorizationEvent is returned for either supported wire. Proof-era
// fields are populated only for ApprovalSerializationVersion. LegacyPayload is
// retained as immutable bytes for legacy consumers and golden verification.
type DecodedAuthorizationEvent struct {
	SerializationVersion string
	EventID              uuid.UUID
	AggregateID          uuid.UUID
	AggregateVersion     int64
	OccurredAt           time.Time
	TraceID              string
	Payload              AuthorizationEvent
	LegacyPayload        json.RawMessage
}
