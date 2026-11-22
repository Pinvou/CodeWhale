// Package usagewaiver contains the narrow wire contract shared by the Token
// coordinator and the Billing financial owner. It owns no business state.
package usagewaiver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Version = 1
const (
	Waived        = "WAIVED"
	Committed     = "COMMITTED"
	ReleasedOther = "RELEASED_OTHER"
)

const Admitted = "ADMITTED"
const Rejected = "REJECTED"

type Decision struct {
	CommandReference uuid.UUID `json:"command_reference"`
	RequestDigest    string    `json:"request_digest"`
	Admission        string    `json:"admission"`
	ReasonCode       string    `json:"reason_code"`
	Result           *Result   `json:"result,omitempty"`
}

func (d Decision) ValidFor(c Command) bool {
	if !c.Valid() || d.CommandReference != c.Reference || d.RequestDigest != c.Digest {
		return false
	}
	if d.Admission == Admitted {
		return d.ReasonCode == "" && (d.Result == nil || d.Result.Matches(c))
	}
	if d.Admission != Rejected || d.Result != nil {
		return false
	}
	switch d.ReasonCode {
	case "EXECUTION_ACTIVE", "USAGE_IDENTITY_MISMATCH", "DEADLINE_RELEASE_OWNS_RECOVERY", "ANOTHER_WAIVER_INTENT", "USAGE_NOT_ELIGIBLE", "USAGE_NOT_FOUND":
		return true
	}
	return false
}

// Command is derived only from Billing's persisted original hold and an
// authenticated/authorized administrator. Digest excludes only itself.
type Command struct {
	Version              int       `json:"version"`
	Reference            uuid.UUID `json:"command_reference"`
	HoldID               uuid.UUID `json:"hold_id"`
	UsageID              uuid.UUID `json:"usage_request_id"`
	WalletID             uuid.UUID `json:"wallet_id"`
	Maximum              int64     `json:"maximum_microcredits,string"`
	AllocationDigest     string    `json:"allocation_digest"`
	ActorID              uuid.UUID `json:"actor_user_id"`
	Issuer               string    `json:"actor_issuer"`
	Subject              string    `json:"actor_subject"`
	AuthorizationVersion int64     `json:"authorization_version,string"`
	Reason               string    `json:"reason"`
	CreatedAt            time.Time `json:"created_at"`
	Digest               string    `json:"request_digest"`
}

func (c Command) Fingerprint() string {
	c.Digest = ""
	raw, _ := json.Marshal(c)
	d := sha256.Sum256(raw)
	return hex.EncodeToString(d[:])
}
func (c Command) Valid() bool {
	return c.Version == Version && c.Reference != uuid.Nil && c.HoldID != uuid.Nil && c.UsageID != uuid.Nil && c.WalletID != uuid.Nil && c.Maximum >= 0 && validDigest(c.AllocationDigest) && c.ActorID != uuid.Nil && c.AuthorizationVersion > 0 && strings.TrimSpace(c.Issuer) != "" && strings.TrimSpace(c.Subject) != "" && !c.CreatedAt.IsZero() && utf8.ValidString(c.Reason) && strings.TrimSpace(c.Reason) != "" && utf8.RuneCountInString(c.Reason) <= 500 && c.Digest == c.Fingerprint()
}

// ExecuteRequest names the existing prepared command. The writer reloads every
// fact from its own immutable row; body actor/amount fields cannot replace it.
type ExecuteRequest struct {
	Reference uuid.UUID `json:"command_reference"`
	Digest    string    `json:"request_digest"`
}

type Result struct {
	CommandReference uuid.UUID       `json:"command_reference"`
	RequestDigest    string          `json:"request_digest"`
	HoldID           uuid.UUID       `json:"hold_id"`
	UsageID          uuid.UUID       `json:"usage_request_id"`
	WalletID         uuid.UUID       `json:"wallet_id"`
	Maximum          int64           `json:"maximum_microcredits,string"`
	Outcome          string          `json:"outcome"`
	HoldStatus       string          `json:"hold_status"`
	Actual           *int64          `json:"actual_microcredits,string,omitempty"`
	UsageSnapshot    json.RawMessage `json:"usage_snapshot"`
	CompletedAt      time.Time       `json:"completed_at"`
}

func (r Result) Matches(c Command) bool {
	if !c.Valid() || r.CommandReference != c.Reference || r.RequestDigest != c.Digest || r.HoldID != c.HoldID || r.UsageID != c.UsageID || r.WalletID != c.WalletID || r.Maximum != c.Maximum || r.CompletedAt.IsZero() || !json.Valid(r.UsageSnapshot) {
		return false
	}
	switch r.Outcome {
	case Waived:
		var proof struct {
			AdminWaiver   bool      `json:"admin_waiver"`
			Reference     uuid.UUID `json:"waiver_command_reference"`
			Digest        string    `json:"waiver_request_digest"`
			ActorID       uuid.UUID `json:"actor_user_id"`
			Issuer        string    `json:"actor_issuer"`
			Subject       string    `json:"actor_subject"`
			Reason        string    `json:"reason"`
			ReleaseReason string    `json:"release_reason"`
		}
		return r.HoldStatus == "RELEASED" && r.Actual != nil && *r.Actual == 0 && json.Unmarshal(r.UsageSnapshot, &proof) == nil && proof.AdminWaiver && proof.Reference == c.Reference && proof.Digest == c.Digest && proof.ActorID == c.ActorID && proof.Issuer == c.Issuer && proof.Subject == c.Subject && proof.Reason == c.Reason && proof.ReleaseReason == "admin_waiver"
	case Committed:
		return r.HoldStatus == "COMMITTED" && r.Actual != nil && *r.Actual >= 0
	case ReleasedOther:
		return r.HoldStatus == "RELEASED" && (r.Actual == nil || *r.Actual == 0)
	}
	return false
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
