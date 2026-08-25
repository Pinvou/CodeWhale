package account

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

var (
	ErrNotFound              = errors.New("account resource not found")
	ErrForbidden             = errors.New("account action forbidden")
	ErrConflict              = errors.New("account resource conflict")
	ErrLastOwner             = errors.New("organization must retain an owner")
	ErrLastActiveSuperAdmin  = errors.New("platform must retain an active super administrator")
	ErrInvalidHierarchy      = errors.New("invalid organization hierarchy")
	ErrInvitationExpired     = errors.New("invitation expired")
	ErrAccountProvisioning   = errors.New("account is provisioning")
	ErrAccountRepairRequired = errors.New("account provisioning requires repair")
	ErrAccountClosing        = errors.New("account closure is in progress")
	ErrBootstrapPending      = errors.New("super administrator bootstrap is pending")
)

type Context struct {
	Type                             string     `json:"context_type"`
	OrganizationID                   *uuid.UUID `json:"organization_id,omitempty"`
	MembershipVersion                int64      `json:"membership_version"`
	ContextVersion                   int64      `json:"context_version"`
	AuthorizationVersion             int64      `json:"authorization_version"`
	PersonalAuthorizationVersion     int64      `json:"personal_authorization_version"`
	PlatformAuthorizationVersion     int64      `json:"platform_authorization_version"`
	OrganizationAuthorizationVersion int64      `json:"organization_authorization_version"`
	Permissions                      []string   `json:"permissions"`
}

type Me struct {
	UserID                 uuid.UUID `json:"user_id"`
	UserCode               string    `json:"user_code"`
	Status                 string    `json:"status"`
	Username               *string   `json:"username"`
	DisplayName            *string   `json:"display_name"`
	Mobile                 *string   `json:"mobile,omitempty"`
	MobileVerified         bool      `json:"mobile_verified"`
	CommerceCustomerStatus string    `json:"commerce_customer_status"`
	ActiveContext          Context   `json:"active_context"`
}

type UnitAuthorizationBinding struct {
	UnitID      uuid.UUID `json:"unit_id"`
	Role        string    `json:"role"`
	Permissions []string  `json:"permissions"`
}

type AuthorizationContext struct {
	UserID                           uuid.UUID                  `json:"user_id"`
	SessionID                        string                     `json:"session_id"`
	ContextType                      string                     `json:"context_type"`
	OrganizationID                   *uuid.UUID                 `json:"organization_id,omitempty"`
	MembershipVersion                int64                      `json:"membership_version"`
	ContextVersion                   int64                      `json:"context_version"`
	AuthorizationVersion             int64                      `json:"authorization_version"`
	PersonalAuthorizationVersion     int64                      `json:"personal_authorization_version"`
	PlatformAuthorizationVersion     int64                      `json:"platform_authorization_version"`
	OrganizationAuthorizationVersion int64                      `json:"organization_authorization_version"`
	UnitIDs                          []uuid.UUID                `json:"unit_ids"`
	AncestorUnitIDs                  []uuid.UUID                `json:"ancestor_unit_ids"`
	Roles                            []string                   `json:"roles"`
	Permissions                      []string                   `json:"permissions"`
	UnitAuthorizationBindings        []UnitAuthorizationBinding `json:"unit_authorization_bindings"`
	PersonalRoles                    []string                   `json:"personal_roles"`
	PersonalPermissions              []string                   `json:"personal_permissions"`
	PlatformRoles                    []string                   `json:"platform_roles"`
	PlatformPermissions              []string                   `json:"platform_permissions"`
	Mobile                           *string                    `json:"mobile,omitempty"`
	MobileVerified                   bool                       `json:"mobile_verified"`
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
	ID             uuid.UUID  `json:"invitation_id"`
	OrganizationID uuid.UUID  `json:"organization_id"`
	TargetType     string     `json:"target_type"`
	TargetHint     string     `json:"target_hint,omitempty"`
	Roles          []string   `json:"roles"`
	Status         string     `json:"status"`
	Version        int64      `json:"version"`
	ExpiresAt      time.Time  `json:"expires_at"`
	AcceptedAt     *time.Time `json:"accepted_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	Token          string     `json:"token,omitempty"`
	Replayed       bool       `json:"replayed"`
}

type InvitationCreateRequest struct {
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

type Repository interface {
	Bootstrap(context.Context, identity.Principal) (Me, bool, error)
	ActivateUser(context.Context, uuid.UUID) error
	GetMe(context.Context, identity.Principal) (Me, error)
	ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error)
	GetCommerceCustomerMapping(context.Context, identity.Principal) (CommerceCustomerMapping, error)
	ListOrganizations(context.Context, identity.Principal) ([]Organization, error)
	SetActiveContext(context.Context, identity.Principal, *uuid.UUID, int64) (Context, error)
	ListUnits(context.Context, identity.Principal, uuid.UUID, *uuid.UUID) (UnitTree, error)
	ListMembers(context.Context, identity.Principal, uuid.UUID, *uuid.UUID) ([]Member, error)
	ListQuotaMembers(context.Context, identity.Principal, uuid.UUID) ([]QuotaMember, error)
	ListMyRoleBindings(context.Context, identity.Principal) ([]RoleBinding, error)
	ListOrganizationRoleBindings(context.Context, identity.Principal, uuid.UUID) ([]RoleBinding, error)
	GrantOrganizationRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error)
	RevokeOrganizationRoleBinding(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (RoleBinding, error)
	ReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string) (Member, error)
	CreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string, string) (UnitMutationResult, error)
	MoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64, string) (UnitMutationResult, error)
	CreateInvitation(context.Context, identity.Principal, uuid.UUID, InvitationCreateRequest) (Invitation, error)
	ListMyInvitations(context.Context, identity.Principal) ([]Invitation, error)
	RevokeInvitation(context.Context, identity.Principal, uuid.UUID, uuid.UUID) (Invitation, error)
	AcceptInvitation(context.Context, identity.Principal, string, *uuid.UUID) (MembershipMutationResult, error)
	RevokeMembership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (MembershipMutationResult, error)
	TransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (OwnershipTransferResult, error)
	Ping(context.Context) error
	Close()
}

type WalletProvisioner interface {
	EnsurePersonalWallet(context.Context, uuid.UUID) error
}
