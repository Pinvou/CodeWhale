package account

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

var (
	ErrNotFound              = errors.New("account resource not found")
	ErrForbidden             = errors.New("account action forbidden")
	ErrConflict              = errors.New("account resource conflict")
	ErrLastActiveSuperAdmin  = errors.New("platform must retain an active super administrator")
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
	PersonalPermissions    []string  `json:"personal_permissions"`
	PlatformPermissions    []string  `json:"platform_permissions"`
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
	OrganizationMembershipVersion    *int64                     `json:"organization_membership_version,omitempty"`
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

type Repository interface {
	Bootstrap(context.Context, identity.Principal) (Me, bool, error)
	GetMe(context.Context, identity.Principal) (Me, error)
	ResolveAuthorizationContext(context.Context, identity.Principal) (AuthorizationContext, error)
	Ping(context.Context) error
	Close()
}

type WalletProvisioner interface {
	EnsurePersonalWallet(context.Context, uuid.UUID) error
}

const PermissionPlatformTokenOwnerCandidatesRead = "platform.tokens.owner-candidates.read"
