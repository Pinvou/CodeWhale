package portal

import (
	"net/http"
	"strings"
)

func (s *Service) proxyAdminToken(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.TokenGatewayURL, "/"+r.PathValue("resource"), tokenAdminPathAllowed)
}

// Each path is an installed management operation, including current usage
// observations. Ordinary model dispatch is not forwarded through this proxy.
func tokenAdminPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet {
		switch resource {
		case "/admin/v1/token-resources/readiness", "/admin/v1/models", "/admin/v1/protocols", "/admin/v1/formulas", "/admin/v1/token-accounts", "/admin/v1/provisionings", "/admin/v1/owner-candidates", "/admin/v1/resource-audit", "/admin/v1/model-catalog/impact/rollback", "/admin/v1/commands/by-idempotency-key", "/admin/v1/commands/recovery-queue", "/admin/v1/usage-requests", "/admin/v1/reconciliation-cases", "/admin/v1/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness", "/admin/v1/overviews/model-catalog", "/admin/v1/overviews/token-resources", "/admin/v1/overviews/model-operations":
			return true
		}
	}
	if method == http.MethodPost && (resource == "/admin/v1/commands" || resource == "/admin/v1/commands:precheck") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || !pathUUID(parts[3]) {
		return false
	}
	if parts[1] == "v2" {
		return method == http.MethodGet && (len(parts) == 4 && (parts[2] == "token-accounts" || parts[2] == "provisionings") || len(parts) == 6 && parts[2] == "token-accounts" && parts[4] == "impact" && parts[5] == "route")
	}
	if parts[1] != "v1" {
		return false
	}
	switch parts[2] {
	case "usage-requests":
		return method == http.MethodGet && (len(parts) == 4 || len(parts) == 5 && parts[4] == "evidence")
	case "models":
		return method == http.MethodGet && (len(parts) == 4 || len(parts) == 5 && (parts[4] == "versions" || parts[4] == "lifecycle-plans") || len(parts) == 6 && parts[4] == "lifecycle-plans" && pathUUID(parts[5]) || len(parts) == 6 && parts[4] == "impact" && parts[5] == "lifecycle") || method == http.MethodPost && (len(parts) == 5 && parts[4] == "lifecycle-plans" || len(parts) == 7 && parts[4] == "lifecycle-plans" && pathUUID(parts[5]) && parts[6] == "cancel")
	case "token-accounts":
		return method == http.MethodGet && (len(parts) == 4 || len(parts) == 5 && (parts[4] == "routes" || parts[4] == "credentials") || len(parts) == 6 && parts[4] == "impact" && (parts[5] == "status" || parts[5] == "route" || parts[5] == "credential")) || method == http.MethodPost && len(parts) == 6 && parts[4] == "credential-launches" && (parts[5] == "enroll" || parts[5] == "rotate")
	case "provisionings":
		return method == http.MethodGet && (len(parts) == 4 || len(parts) == 5 && parts[4] == "attempts")
	case "formulas":
		return method == http.MethodGet && len(parts) == 5 && parts[4] == "preview"
	case "credential-results":
		return method == http.MethodGet && len(parts) == 4 || method == http.MethodPost && len(parts) == 5 && parts[4] == "revoke-window"
	case "commands":
		return method == http.MethodGet && (len(parts) == 4 || len(parts) == 6 && parts[4] == "recovery-actions" && (parts[5] == "by-idempotency-key" || pathUUID(parts[5]))) || method == http.MethodPost && len(parts) == 5 && parts[4] == "recovery-actions"
	}
	return false
}

func pricingAdminPathAllowed(method, resource string) bool {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet && (resource == "/admin/v1/pricing" || resource == "/admin/v1/metering-units" || resource == "/admin/v1/finance-overviews/pricing") {
		return true
	}
	if method == http.MethodPost && (resource == "/admin/v1/pricing/drafts" || resource == "/admin/v1/metering-units") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" || !pathUUID(parts[3]) {
		return false
	}
	if parts[2] == "models" {
		return method == http.MethodGet && len(parts) == 5 && parts[4] == "price-projection"
	}
	if parts[2] == "metering-units" {
		return method == http.MethodPost && len(parts) == 5 && parts[4] == "deactivate"
	}
	if parts[2] != "pricing" {
		return false
	}
	return method == http.MethodGet && (len(parts) == 4 || len(parts) == 5 && parts[4] == "preview") || method == http.MethodPut && len(parts) == 5 && parts[4] == "draft" || method == http.MethodPost && len(parts) == 5 && (parts[4] == "publish" || parts[4] == "transition")
}
