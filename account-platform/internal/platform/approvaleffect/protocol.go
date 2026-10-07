package approvaleffect

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

const SchemaVersion = 1

const (
	AuthorityBilling = "BILLING"
	StateAbsent      = "ABSENT"
	StateObserved    = "EFFECT_OR_ATTEMPT_OBSERVED"
)

var ErrInvalidObservation = errors.New("invalid approval effect observation")

// Observation is a closed, read-only authority response. OBSERVED is
// deliberately conservative: it may be an attempt, a result, or an effect and
// therefore can never authorize request invalidation.
type Observation struct {
	SchemaVersion      int       `json:"schema_version"`
	Authority          string    `json:"authority"`
	ApprovalRequestID  uuid.UUID `json:"approval_request_id"`
	CommandType        string    `json:"command_type"`
	CommandFingerprint string    `json:"command_fingerprint"`
	State              string    `json:"state"`
	ObservedAt         time.Time `json:"observed_at"`
}

func AuthorityForCommand(commandType string) (string, bool) {
	for _, descriptor := range approvalconsumer.All() {
		if descriptor.CommandType != commandType {
			continue
		}
		switch descriptor.Workload {
		case approvalconsumer.BillingWorkload:
			return AuthorityBilling, true
		}
	}
	return "", false
}

func ValidFingerprint(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (observation Observation) Validate() error {
	expectedAuthority, known := AuthorityForCommand(observation.CommandType)
	if observation.SchemaVersion != SchemaVersion || !known || observation.Authority != expectedAuthority ||
		observation.ApprovalRequestID == uuid.Nil || !ValidFingerprint(observation.CommandFingerprint) ||
		(observation.State != StateAbsent && observation.State != StateObserved) || observation.ObservedAt.IsZero() ||
		observation.ObservedAt.Location() != time.UTC {
		return ErrInvalidObservation
	}
	return nil
}
