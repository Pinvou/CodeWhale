package account

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"time"
)

type AdminOrganization struct {
	ID          uuid.UUID `json:"organization_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	MemberCount int64     `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type AdminOrganizationFilter struct {
	Search          string
	Status          string
	Limit           int
	CursorUpdatedAt *time.Time
	CursorID        *uuid.UUID
}

type AdminOrganizationPage struct {
	Items      []AdminOrganization `json:"items"`
	NextCursor *string             `json:"next_cursor"`
}

type AdminMemberFilter struct {
	Search         string
	Role           string
	Limit          int
	CursorJoinedAt *time.Time
	CursorID       *uuid.UUID
}

type AdminMemberPage struct {
	Items      []Member `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

type AdminOrganizationState struct {
	ID      uuid.UUID `json:"organization_id"`
	Status  string    `json:"status"`
	Version int64     `json:"version"`
}

type AdminRoleDefinition struct {
	Code        string   `json:"code"`
	Scope       string   `json:"scope"`
	Assignable  bool     `json:"assignable"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
}

type AdminUserMembership struct {
	OrganizationID   uuid.UUID   `json:"organization_id"`
	OrganizationName string      `json:"organization_name"`
	MembershipStatus string      `json:"membership_status"`
	Roles            []string    `json:"roles"`
	UnitAdminIDs     []uuid.UUID `json:"unit_admin_unit_ids"`
}

type organizationAdminRepository interface {
	AdminRoleBindings(context.Context, *uuid.UUID, *uuid.UUID) ([]RoleBinding, error)
	AdminGrantRoleBinding(context.Context, identity.Principal, RoleBindingGrant) (RoleBinding, error)
	AdminRevokeRoleBinding(context.Context, identity.Principal, uuid.UUID) (RoleBinding, error)
	AdminUserMemberships(context.Context, uuid.UUID) ([]AdminUserMembership, error)
	AdminOrganizations(context.Context, AdminOrganizationFilter) (AdminOrganizationPage, error)
	AdminCreateOrganization(context.Context, identity.Principal, uuid.UUID, string, string) (Organization, error)
	AdminRecordOrganizationCreationFailure(context.Context, identity.Principal, uuid.UUID, string, string) error
	AdminOrganizationState(context.Context, uuid.UUID) (AdminOrganizationState, error)
	AdminOrganizationDeletionBlockers(context.Context, uuid.UUID) ([]OrganizationLifecycleBlocker, error)
	AdminRenameOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminSetOrganizationStatus(context.Context, identity.Principal, uuid.UUID, string) error
	AdminDeleteOrganization(context.Context, identity.Principal, uuid.UUID, string) error
	AdminUnits(context.Context, uuid.UUID) (UnitTree, error)
	AdminUnitState(context.Context, uuid.UUID, uuid.UUID) (UnitMutationResult, error)
	AdminMembers(context.Context, uuid.UUID, AdminMemberFilter) (AdminMemberPage, error)
	AdminAddMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID, string) (MembershipMutationResult, error)
	AdminReplaceMemberRoles(context.Context, identity.Principal, uuid.UUID, uuid.UUID, []string, []uuid.UUID) (Member, error)
	AdminRemoveMember(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (MembershipMutationResult, error)
	AdminTransferOwnership(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (OwnershipTransferResult, error)
	AdminCreateUnit(context.Context, identity.Principal, uuid.UUID, *uuid.UUID, string, string) (UnitMutationResult, error)
	AdminRenameUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string, string) (UnitMutationResult, error)
	AdminMoveUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, *uuid.UUID, int64, string) (UnitMutationResult, error)
	AdminUnitDeletionBlockers(context.Context, uuid.UUID, uuid.UUID) ([]OrganizationLifecycleBlocker, error)
	AdminDeleteUnit(context.Context, identity.Principal, uuid.UUID, uuid.UUID, string) (UnitMutationResult, error)
}
