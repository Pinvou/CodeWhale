package billing

import (
	"github.com/google/uuid"
	"time"
)

type MemberQuotaGrant struct {
	ID                    uuid.UUID `json:"grant_id"`
	OrganizationID        uuid.UUID `json:"organization_id"`
	MemberUserID          uuid.UUID `json:"member_user_id"`
	PackageID             uuid.UUID `json:"package_id"`
	ComponentID           uuid.UUID `json:"component_id"`
	ComponentType         string    `json:"component_type"`
	OriginalMicrocredits  int64     `json:"original_microcredits"`
	AvailableMicrocredits int64     `json:"available_microcredits"`
	ReservedMicrocredits  int64     `json:"reserved_microcredits"`
	UsedMicrocredits      int64     `json:"used_microcredits"`
	ExpiredMicrocredits   int64     `json:"expired_microcredits"`
	RevokedMicrocredits   int64     `json:"revoked_microcredits"`
	Status                string    `json:"status"`
	SuspensionReason      *string   `json:"suspension_reason,omitempty"`
	MembershipVersion     int64     `json:"membership_version"`
	AllocationVersion     int64     `json:"allocation_version"`
	CreatedBy             uuid.UUID `json:"created_by"`
	ExpiresAt             time.Time `json:"expires_at"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type MemberQuotaAllocation struct {
	OrganizationID     uuid.UUID `json:"organization_id"`
	MemberUserID       uuid.UUID `json:"member_user_id"`
	PackageID          uuid.UUID `json:"package_id"`
	ComponentID        uuid.UUID `json:"component_id"`
	ComponentType      string    `json:"component_type"`
	AmountMicrocredits int64     `json:"amount_microcredits"`
	MembershipVersion  int64     `json:"membership_version"`
	CreatedBy          uuid.UUID `json:"created_by"`
	IdempotencyKey     string    `json:"idempotency_key"`
}
