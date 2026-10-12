package token

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func (s *Service) registerOrganizationFinanceRoutes(routes *httpserver.RouteGroup) {
	// Personal finance has no organization/model repository. Keep organization
	// facilities in the existing explicit management assembly.
	if !s.managementEnabled {
		return
	}
	routes.GET("/organizations/:organization_id/model-policy", httpserver.AdaptFunc(s.organizationModelPolicy))
	routes.PUT("/organizations/:organization_id/model-policy", approvalpolicy.Require("TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationModelPolicy))
	routes.GET("/organizations/:organization_id/balance", httpserver.AdaptFunc(s.getOrganizationBalance))
	routes.GET("/organizations/:organization_id/entitlement-packages", httpserver.AdaptFunc(s.getOrganizationEntitlementPackages))
	routes.GET("/organizations/:organization_id/recharge/products", httpserver.AdaptFunc(s.getOrganizationRechargeProducts))
	routes.GET("/organizations/:organization_id/payment-capabilities", httpserver.AdaptFunc(s.getOrganizationPaymentCapabilities))
	routes.POST("/organizations/:organization_id/recharge/orders", httpserver.AdaptFunc(s.createOrganizationRechargeOrder))
	routes.GET("/organizations/:organization_id/recharge/orders/:order_id", httpserver.AdaptFunc(s.getOrganizationRechargeOrder))
	routes.GET("/organizations/:organization_id/quota-grants", httpserver.AdaptFunc(s.listOrganizationQuotaGrants))
	routes.POST("/organizations/:organization_id/quota-grants:batch", httpserver.AdaptFunc(s.allocateOrganizationQuotaBatch))
	routes.POST("/organizations/:organization_id/quota-grants/:grant_id/revoke", httpserver.AdaptFunc(s.revokeOrganizationQuotaGrant))
}
