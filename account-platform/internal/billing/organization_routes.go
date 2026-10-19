package billing

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

func (s *Service) registerOrganizationRoutes(protected *httpserver.RouteGroup) {
	protected.PUT("/internal/v1/organizations/:organization_id/lifecycle", serviceauth.Require("internal.billing.organization.lifecycle"), approvalpolicy.Require("BILLING_ORGANIZATION_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationLifecycle))
	protected.PUT("/internal/v1/organizations/:organization_id/units/:unit_id/lifecycle", serviceauth.Require("internal.billing.organization-unit.lifecycle"), approvalpolicy.Require("BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationUnitDeletionBlockers))
	protected.PUT("/internal/v1/organizations/:organization_id/members/:user_id/lifecycle", serviceauth.Require("internal.billing.organization-member.lifecycle"), approvalpolicy.Require("BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationMemberLifecycle))
	protected.GET("/internal/v1/organizations/:organization_id/quota-grants", serviceauth.Require("internal.billing.quota.read"), httpserver.AdaptFunc(s.listMemberQuotaGrants))
	protected.POST("/internal/v1/organizations/:organization_id/quota-grants:batch", serviceauth.Require("internal.billing.quota.allocate"), s.requireHumanDelegation("org.quota.allocate", "organization", "organization_id"), httpserver.AdaptFunc(s.allocateMemberQuotaBatch))
	protected.POST("/internal/v1/organizations/:organization_id/quota-grants/:grant_id/revoke", serviceauth.Require("internal.billing.quota.revoke"), s.requireHumanDelegation("org.quota.revoke", "member-quota-grant", "grant_id"), httpserver.AdaptFunc(s.revokeMemberQuotaGrant))
}
