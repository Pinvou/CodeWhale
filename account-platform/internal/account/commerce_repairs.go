package account

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

var ErrInvalidCommerceRepair = errors.New("invalid commerce repair request")

// References identify controlled records. Documents and credentials remain in
// their source systems; neither an opaque reference nor its digest is a
// machine-verified provider result. Independent humans attest that result.
var commerceEvidenceReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,159}$`)
var commerceEvidenceDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var commerceRedactedReference = regexp.MustCompile(`^\*{3}[A-Za-z0-9]{1,8}$`)

func commerceRepairInspectableJobStatus(status string) bool {
	return status == "REPAIR_REQUIRED" || status == "CANCELLED_ACCOUNT_CLOSURE" || status == "LEASED" || status == "RETRY"
}

type CommerceRepairCase struct {
	ID                           uuid.UUID  `json:"id"`
	JobID                        uuid.UUID  `json:"job_id"`
	MappingID                    uuid.UUID  `json:"mapping_id"`
	MappingVersion               int64      `json:"mapping_version"`
	SubjectType                  string     `json:"subject_type"`
	SubjectID                    uuid.UUID  `json:"subject_id"`
	SubjectLifecycleVersion      int64      `json:"subject_lifecycle_version"`
	ProviderID                   string     `json:"provider_id"`
	AdapterVersion               string     `json:"adapter_version"`
	TaskCategory                 string     `json:"task_category"`
	Attempts                     int        `json:"attempts"`
	Version                      int64      `json:"version"`
	Status                       string     `json:"status"`
	Resolution                   *string    `json:"resolution,omitempty"`
	ApprovedObservationID        *uuid.UUID `json:"approved_observation_id,omitempty"`
	ReviewerUserID               *uuid.UUID `json:"reviewer_user_id,omitempty"`
	ReviewerBindingID            *uuid.UUID `json:"-"`
	ReviewerAuthorizationVersion *int64     `json:"-"`
	CreatedAt                    time.Time  `json:"created_at"`
	ResolvedAt                   *time.Time `json:"resolved_at,omitempty"`
}

type CommerceRepairObservationInput struct {
	ExpectedVersion           int64     `json:"expected_version"`
	ObservationID             uuid.UUID `json:"observation_id"`
	Outcome                   string    `json:"outcome"`
	EvidenceReference         string    `json:"evidence_reference"`
	EvidenceSHA256            string    `json:"evidence_sha256"`
	AllAttemptsEnded          bool      `json:"all_attempts_ended"`
	ExternalReferenceRedacted string    `json:"external_reference_redacted"`
}

func (v CommerceRepairObservationInput) valid() bool {
	return v.ExpectedVersion > 0 && v.ObservationID != uuid.Nil &&
		(v.Outcome == "UNCONFIRMED" || v.Outcome == "SUCCEEDED" || v.Outcome == "ENDED_NO_EFFECT") &&
		commerceEvidenceReference.MatchString(v.EvidenceReference) && commerceEvidenceDigest.MatchString(v.EvidenceSHA256) &&
		(v.Outcome == "UNCONFIRMED" || v.AllAttemptsEnded) &&
		(v.ExternalReferenceRedacted == "" || v.Outcome == "SUCCEEDED" && commerceRedactedReference.MatchString(v.ExternalReferenceRedacted))
}

type CommerceRepairObservation struct {
	ID                           uuid.UUID `json:"id"`
	CaseID                       uuid.UUID `json:"case_id"`
	CaseVersion                  int64     `json:"case_version"`
	Outcome                      string    `json:"outcome"`
	EvidenceReference            string    `json:"evidence_reference"`
	EvidenceSHA256               string    `json:"evidence_sha256"`
	AllAttemptsEnded             bool      `json:"all_attempts_ended"`
	ExternalReferenceRedacted    string    `json:"external_reference_redacted,omitempty"`
	ProposerUserID               uuid.UUID `json:"proposer_user_id"`
	ProposerBindingID            uuid.UUID `json:"-"`
	ProposerAuthorizationVersion int64     `json:"-"`
	CreatedAt                    time.Time `json:"created_at"`
}

func (v CommerceRepairObservation) matches(caseID, actor uuid.UUID, in CommerceRepairObservationInput) bool {
	return v.CaseID == caseID && v.ProposerUserID == actor && v.CaseVersion == in.ExpectedVersion+1 &&
		v.Outcome == in.Outcome && v.EvidenceReference == in.EvidenceReference && v.EvidenceSHA256 == in.EvidenceSHA256 &&
		v.AllAttemptsEnded == in.AllAttemptsEnded && v.ExternalReferenceRedacted == in.ExternalReferenceRedacted
}

type CommerceRepairConfirmation struct {
	ExpectedVersion int64     `json:"expected_version"`
	ObservationID   uuid.UUID `json:"observation_id"`
}

type CommerceRepairDetail struct {
	Case             CommerceRepairCase          `json:"case"`
	Observations     []CommerceRepairObservation `json:"observations"`
	MoreObservations bool                        `json:"more_observations"`
	Callbacks        []CommerceRepairCallback    `json:"callbacks"`
	MoreCallbacks    bool                        `json:"more_callbacks"`
}

type CommerceRepairCallback struct {
	ID                        uuid.UUID `json:"id"`
	Sequence                  int64     `json:"sequence"`
	CaseID                    uuid.UUID `json:"case_id"`
	Attempts                  int       `json:"attempts"`
	Kind                      string    `json:"kind"`
	ExternalReferenceRedacted string    `json:"external_reference_redacted,omitempty"`
	AfterCompletion           bool      `json:"after_completion"`
	RecordedAt                time.Time `json:"recorded_at"`
}

type CommerceRepairRepository interface {
	CommerceRepairCases(context.Context, identity.Principal, uuid.UUID, int) ([]CommerceRepairCase, error)
	CommerceRepairDetail(context.Context, identity.Principal, uuid.UUID, int64, int64) (CommerceRepairDetail, error)
	QuarantineCommerceRepair(context.Context, identity.Principal, uuid.UUID) (CommerceRepairCase, error)
	ObserveCommerceRepair(context.Context, identity.Principal, uuid.UUID, CommerceRepairObservationInput) (CommerceRepairObservation, error)
	ConfirmCommerceRepair(context.Context, identity.Principal, uuid.UUID, CommerceRepairConfirmation) (CommerceRepairCase, error)
}
