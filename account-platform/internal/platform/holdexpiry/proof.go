package holdexpiry

import (
	"github.com/google/uuid"
	"time"
)

// Identity freezes the reservation, not a caller's assertion about dispatch.
type Identity struct {
	UsageRequestID  uuid.UUID `json:"usage_request_id"`
	HoldID          uuid.UUID `json:"hold_id"`
	WalletID        uuid.UUID `json:"wallet_id"`
	MaxMicrocredits int64     `json:"max_microcredits,string"`
	ExpiresAt       time.Time `json:"expires_at"`
}
type Proof struct {
	Identity
	ProofID uuid.UUID `json:"proof_id"`
	Status  string    `json:"status"`
}

func (i Identity) Valid() bool {
	return i.UsageRequestID != uuid.Nil && i.HoldID != uuid.Nil && i.WalletID != uuid.Nil && i.MaxMicrocredits >= 0 && !i.ExpiresAt.IsZero()
}
func (i Identity) Matches(other Identity) bool {
	return i.UsageRequestID == other.UsageRequestID && i.HoldID == other.HoldID && i.WalletID == other.WalletID && i.MaxMicrocredits == other.MaxMicrocredits && i.ExpiresAt.Equal(other.ExpiresAt)
}
func (p Proof) Matches(i Identity) bool {
	return p.Identity.Matches(i) && p.ProofID != uuid.Nil && p.Status == "LOCAL_CONFIRMED"
}
