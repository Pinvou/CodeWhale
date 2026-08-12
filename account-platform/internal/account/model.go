package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

var (
	ErrNotFound          = errors.New("account resource not found")
	ErrForbidden         = errors.New("account action forbidden")
	ErrConflict          = errors.New("account resource conflict")
	ErrLastOwner         = errors.New("organization must retain an owner")
	ErrInvalidHierarchy  = errors.New("invalid organization hierarchy")
	ErrInvitationExpired = errors.New("invitation expired")
)

type Context struct {
	Type              string     `json:"context_type"`
	OrganizationID    *uuid.UUID `json:"organization_id,omitempty"`
	MembershipVersion int64      `json:"membership_version"`
}

type Me struct {
	UserID        uuid.UUID `json:"user_id"`
	Status        string    `json:"status"`
	DisplayName   *string   `json:"display_name"`
	ActiveContext Context   `json:"active_context"`
}

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

type Invitation struct {
	ID             uuid.UUID `json:"invitation_id"`
	OrganizationID uuid.UUID `json:"organization_id"`
	Email          string    `json:"email"`
	Roles          []string  `json:"roles"`
	ExpiresAt      time.Time `json:"expires_at"`
	Token          string    `json:"token,omitempty"`
}

type Repository interface {
	Bootstrap(context.Context, identity.Principal) (Me, bool, error)
	ActivateUser(context.Context, uuid.UUID) error
	GetMe(context.Context, identity.Principal) (Me, error)
	ListOrganizations(context.Context, identity.Principal) ([]Organization, error)
	SetActiveContext(context.Context, identity.Principal, *uuid.UUID) (Context, error)
	CreateOrganization(context.Context, identity.Principal, string) (Organization, error)
	CreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string) (Unit, error)
	MoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64) (Unit, error)
	CreateInvitation(context.Context, identity.Principal, uuid.UUID, string, []string, time.Time) (Invitation, error)
	AcceptInvitation(context.Context, identity.Principal, string) (Organization, error)
	RevokeMembership(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	TransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID) error
	Ping(context.Context) error
	Close()
}

type WalletProvisioner interface {
	EnsurePersonalWallet(context.Context, uuid.UUID) error
}
