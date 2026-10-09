package token

import (
	"errors"
	"github.com/google/uuid"
)

var (
	ErrForbidden                   = errors.New("token account is not authorized")
	ErrAccountProvisioning         = errors.New("account is provisioning")
	ErrAccountRepairRequired       = errors.New("account provisioning requires repair")
	ErrAccountAuthorityUnavailable = errors.New("account authority is unavailable")
)

type AuthorizationContext struct {
	UserID                        uuid.UUID
	Mobile                        *string
	MobileVerified                bool
	SessionID                     string
	ContextType                   string
	OrganizationID                *uuid.UUID
	ContextVersion                int64
	AuthorizationVersion          int64
	MembershipVersion             int64
	OrganizationMembershipVersion *int64
	UnitIDs                       []uuid.UUID
	AncestorUnitIDs               []uuid.UUID
	UnitAuthorizationBindings     []UnitAuthorizationBinding
	Roles                         []string
	Permissions                   []string
}
