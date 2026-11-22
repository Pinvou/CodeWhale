package account

import (
	"errors"
	"github.com/google/uuid"
	"time"
)

type Organization struct {
	ID      uuid.UUID `json:"organization_id"`
	Name    string    `json:"name"`
	Status  string    `json:"status"`
	Version int64     `json:"version"`
	Roles   []string  `json:"roles,omitempty"`
}

type Unit struct {
	ID             uuid.UUID  `json:"unit_id"`
	OrganizationID uuid.UUID  `json:"organization_id"`
	ParentID       *uuid.UUID `json:"parent_id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	Version        int64      `json:"version"`
}

type UnitMutationResult struct {
	Unit             Unit  `json:"unit"`
	StructureVersion int64 `json:"structure_version"`
	Replayed         bool  `json:"replayed"`
}

type UnitTree struct {
	Items            []Unit `json:"items"`
	StructureVersion int64  `json:"structure_version"`
}

type Member struct {
	UserID       uuid.UUID   `json:"user_id"`
	UserCode     string      `json:"user_code"`
	DisplayName  *string     `json:"display_name"`
	Email        *string     `json:"email"`
	Status       string      `json:"status"`
	Version      int64       `json:"version"`
	Roles        []string    `json:"roles"`
	UnitIDs      []uuid.UUID `json:"unit_ids"`
	UnitAdminIDs []uuid.UUID `json:"unit_admin_unit_ids"`
	JoinedAt     time.Time   `json:"joined_at"`
	MembershipID uuid.UUID   `json:"membership_id"`
}

type QuotaMember struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName *string   `json:"display_name"`
	Version     int64     `json:"version"`
}

type Invitation struct {
	UnitIDs          []uuid.UUID `json:"unit_ids,omitempty"`
	ID               uuid.UUID   `json:"invitation_id"`
	OrganizationID   uuid.UUID   `json:"organization_id"`
	TargetType       string      `json:"target_type"`
	OrganizationName string      `json:"organization_name,omitempty"`
	TargetHint       string      `json:"target_hint,omitempty"`
	Roles            []string    `json:"roles"`
	Status           string      `json:"status"`
	Version          int64       `json:"version"`
	ExpiresAt        time.Time   `json:"expires_at"`
	AcceptedAt       *time.Time  `json:"accepted_at,omitempty"`
	RevokedAt        *time.Time  `json:"revoked_at,omitempty"`
	Token            string      `json:"token,omitempty"`
	Replayed         bool        `json:"replayed"`
}

type InvitationCreateRequest struct {
	UnitIDs        []uuid.UUID
	TargetType     string
	Target         string
	Roles          []string
	ExpiresInHours int
	IdempotencyKey string
}

type MembershipMutationResult struct {
	OrganizationID uuid.UUID `json:"organization_id"`
	Member         Member    `json:"member"`
	Replayed       bool      `json:"replayed"`
}

type OwnershipTransferResult struct {
	OrganizationID     uuid.UUID `json:"organization_id"`
	PreviousOwnerID    uuid.UUID `json:"previous_owner_user_id"`
	NewOwnerID         uuid.UUID `json:"new_owner_user_id"`
	NewOwnerMembership Member    `json:"new_owner_membership"`
	Replayed           bool      `json:"replayed"`
}

var (
	ErrInvitationExpired = errors.New("invitation expired")
	ErrLastOwner         = errors.New("organization must retain an owner")
	ErrInvalidHierarchy  = errors.New("invalid organization hierarchy")
)
