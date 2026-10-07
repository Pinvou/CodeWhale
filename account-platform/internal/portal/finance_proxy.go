package portal

import (
	"net/http"
	"strings"
)

func currentGovernedApprovalCommand(command string) bool {
	return command == "WALLET_ADJUSTMENT" || command == "PAYMENT_PROVIDER_RESUME" || command == "PAYMENT_PRODUCT_RESUME" || command == "TOKEN_MODEL_LIFECYCLE_CHANGE" || command == "TOKEN_MODEL_CATALOG_ROLLBACK" || command == "TOKEN_CREDENTIAL_REVOKE"
}

func (s *Service) proxyAdminBilling(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.BillingURL, "/"+r.PathValue("resource"), billingAdminPathAllowed)
}

// This stage exposes payment/entitlement operations and the three actual
// governed commands, plus F03 reconciliation reads/imports/case commands.
// Usage repair and later governance paths remain outside this stage.
func billingAdminPathAllowed(method, resource string) bool {
	if pricingAdminPathAllowed(method, resource) {
		return true
	}
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if method == http.MethodGet {
		switch resource {
		case "/admin/v1/reconciliation-runs", "/admin/v1/reconciliation-cases", "/admin/v1/overview", "/admin/v1/finance-overviews/accounts", "/admin/v1/finance-overviews/payments", "/admin/v1/finance-overviews/products", "/admin/v1/wallets", "/admin/v1/receivables", "/admin/v1/recharge-products", "/admin/v1/recharge-product-create-options", "/admin/v1/payment-orders", "/admin/v1/refunds", "/admin/v1/payment-provider-controls", "/admin/v1/late-payment-reviews", "/admin/v1/payment-anomalies", "/admin/v1/adjustments":
			return true
		}
	}
	if method == http.MethodPost && (resource == "/admin/v1/reconciliation-runs" || resource == "/admin/v1/reconciliation-imports" || resource == "/admin/v1/recharge-products" || resource == "/admin/v1/adjustments") {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 4 || parts[0] != "admin" || parts[1] != "v1" || !pathUUID(parts[3]) {
		return false
	}
	if len(parts) == 4 && method == http.MethodGet {
		return parts[2] == "payment-orders" || parts[2] == "late-payment-reviews" || parts[2] == "payment-anomalies"
	}
	if len(parts) == 5 {
		if method == http.MethodGet {
			return parts[2] == "reconciliation-runs" && parts[4] == "cases" || parts[2] == "wallets" && parts[4] == "ledger" || parts[2] == "receivables" && parts[4] == "allocations" || parts[2] == "recharge-products" && parts[4] == "admission-verifications"
		}
		if method == http.MethodPut {
			return parts[2] == "recharge-products" && parts[4] == "draft"
		}
		if method != http.MethodPost {
			return false
		}
		switch parts[2] {
		case "reconciliation-cases":
			return parts[4] == "commands"
		case "payment-orders":
			return parts[4] == "sync" || parts[4] == "repair" || parts[4] == "refunds"
		case "late-payment-reviews":
			return parts[4] == "claim" || parts[4] == "query" || parts[4] == "schedule" || parts[4] == "archive" || parts[4] == "provider-blind"
		case "payment-anomalies":
			return parts[4] == "claim" || parts[4] == "resolve"
		case "adjustments":
			return parts[4] == "reject"
		case "recharge-products":
			return parts[4] == "admission-verifications" || parts[4] == "publish" || parts[4] == "unpublish" || parts[4] == "delete" || parts[4] == "display-names"
		}
	}
	return len(parts) == 7 && parts[2] == "recharge-products" && parts[4] == "admission-verifications" && pathUUID(parts[5]) && method == http.MethodPost && (parts[6] == "retry" || parts[6] == "revoke")
}
