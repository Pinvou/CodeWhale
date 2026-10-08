package account

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func (s *Service) registerOrganizationRoutes(protected, admin *httpserver.RouteGroup) {
	protected.GET("/v1/me/role-bindings", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.listMyRoleBindings))
	protected.GET("/v1/me/organizations", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.listOrganizations))
	protected.POST("/v1/sessions/current/active-context", s.requirePermission(PermissionPersonalContextSwitch), httpserver.AdaptFunc(s.setActiveContext))
	protected.GET("/v1/organizations/:organization_id/units", httpserver.AdaptFunc(s.listUnits))
	protected.POST("/v1/organizations/:organization_id/units", httpserver.AdaptFunc(s.createUnit))
	protected.GET("/v1/organizations/:organization_id/members", httpserver.AdaptFunc(s.listMembers))
	protected.GET("/v1/organizations/:organization_id/quota-members", httpserver.AdaptFunc(s.listQuotaMembers))
	protected.GET("/v1/organizations/:organization_id/role-bindings", httpserver.AdaptFunc(s.listOrganizationRoleBindings))
	protected.POST("/v1/organizations/:organization_id/role-bindings", httpserver.AdaptFunc(s.grantOrganizationRoleBinding))
	protected.DELETE("/v1/organizations/:organization_id/role-bindings/:binding_id", httpserver.AdaptFunc(s.revokeOrganizationRoleBinding))
	protected.PUT("/v1/organizations/:organization_id/members/:user_id/roles", httpserver.AdaptFunc(s.replaceMemberRoles))
	protected.PATCH("/v1/organizations/:organization_id/units/:unit_id/parent", httpserver.AdaptFunc(s.moveUnit))
	protected.POST("/v1/organizations/:organization_id/invitations", httpserver.AdaptFunc(s.createInvitation))
	protected.GET("/v1/me/invitations", s.requirePermission(PermissionPersonalInvitationsRead), httpserver.AdaptFunc(s.listMyInvitations))
	protected.DELETE("/v1/organizations/:organization_id/invitations/:invitation_id", httpserver.AdaptFunc(s.revokeInvitation))
	protected.POST("/v1/invitations/accept", s.requirePermission(PermissionPersonalInvitationsAccept), httpserver.AdaptFunc(s.acceptInvitation))
	protected.DELETE("/v1/organizations/:organization_id/members/:user_id", httpserver.AdaptFunc(s.revokeMembership))
	protected.POST("/v1/organizations/:organization_id/ownership", httpserver.AdaptFunc(s.transferOwnership))
	admin.GET("/users/:user_id/memberships", s.requirePermission(PermissionPlatformUsersRead), httpserver.AdaptFunc(s.adminUserMemberships))
	admin.GET("/organizations", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminOrganizations))
	admin.POST("/organizations", s.requirePermission(PermissionPlatformOrganizationsCreate), approvalpolicy.Require("ACCOUNT_ORGANIZATION_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateOrganization))
	admin.PATCH("/organizations/:organization_id", s.requirePermission(PermissionPlatformOrganizationsProfileManage), approvalpolicy.Require("ACCOUNT_ORGANIZATION_RENAME", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRenameOrganization))
	admin.PATCH("/organizations/:organization_id/status", s.requirePermission(PermissionPlatformOrganizationsStatusManage), approvalpolicy.Require("ACCOUNT_ORGANIZATION_STATUS_CHANGE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSetOrganizationStatus))
	admin.GET("/organizations/:organization_id/deletion-readiness", s.requirePermission(PermissionPlatformOrganizationsDelete), httpserver.AdaptFunc(s.adminOrganizationDeletionReadiness))
	admin.DELETE("/organizations/:organization_id", s.requirePermission(PermissionPlatformOrganizationsDelete), approvalpolicy.Require("ACCOUNT_ORGANIZATION_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteOrganization))
	admin.GET("/organizations/:organization_id/units", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminUnits))
	admin.GET("/organizations/:organization_id/members", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminMembers))
	admin.POST("/organizations/:organization_id/members", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_ADD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminAddMember))
	admin.PUT("/organizations/:organization_id/members/:user_id/roles", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_ROLES_REPLACE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminReplaceMemberRoles))
	admin.DELETE("/organizations/:organization_id/members/:user_id", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_REMOVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRemoveMember))
	admin.POST("/organizations/:organization_id/ownership", s.requirePermission(PermissionPlatformOrganizationsOwnerTransfer), approvalpolicy.Require("ACCOUNT_OWNERSHIP_TRANSFER", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminTransferOwnership))
	admin.POST("/organizations/:organization_id/units", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateUnit))
	admin.PATCH("/organizations/:organization_id/units/:unit_id", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_RENAME", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRenameUnit))
	admin.PATCH("/organizations/:organization_id/units/:unit_id/parent", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_MOVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminMoveUnit))
	admin.DELETE("/organizations/:organization_id/units/:unit_id", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteUnit))
}
