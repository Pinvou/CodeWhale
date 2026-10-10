package portal

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Only the authenticated import endpoint accepts the large bounded bill body.
const reconciliationImportBodyLimit int64 = 64 << 20

func currentGovernedApprovalCommand(command string) bool {
	return command == "USAGE_SETTLEMENT_REPAIR" || command == "RECONCILIATION_FINANCIAL_REPAIR" || command == "CHARGEBACK_FINANCIAL_REPAIR" || command == "REFUND_EXCEPTION_REPAIR" || command == "PAYMENT_FINANCIAL_REPAIR" || command == "RECEIVABLE_WRITE_OFF" || command == "LEDGER_REPAIR" || command == "ENTITLEMENT_COMPONENT_ADJUSTMENT" || command == "WALLET_ADJUSTMENT" || command == "PAYMENT_PROVIDER_RESUME" || command == "PAYMENT_PRODUCT_RESUME" || command == "TOKEN_MODEL_LIFECYCLE_CHANGE" || command == "TOKEN_MODEL_CATALOG_ROLLBACK" || command == "TOKEN_CREDENTIAL_REVOKE"
}

func (s *Service) proxyAdminBilling(w http.ResponseWriter, r *http.Request) {
	s.proxy(w, r, s.config.BillingURL, "/"+r.PathValue("resource"), billingAdminPathAllowed)
}

// Expose delivered payment, entitlement, receivable and governance operations,
// plus reconciliation reads/imports/case commands. Undelivered commands remain
// excluded by the explicit allowlists below.
func billingAdminPathAllowed(method, resource string) bool {
	if resource == "/admin/v1/registration-bonus-policy" {
		return method == http.MethodGet || method == http.MethodPut
	}
	if pricingAdminPathAllowed(method, resource) {
		return true
	}
	if method == http.MethodHead {
		method = http.MethodGet
	}
	if resource == "/admin/v1/registration-bonus-policy" && (method == http.MethodGet || method == http.MethodPut) {
		return true
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

// Preflight the sole large upload against current Account facts before reading
// its body. Billing repeats authorization at its own boundary after forwarding.
func (s *Service) allowReconciliationImport(w http.ResponseWriter, r *http.Request, session Session) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	target := *s.config.AccountURL
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/me"
	target.RawQuery = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return reconciliationPreflightUnavailable(w, r)
	}
	request.Header.Set("Authorization", "Bearer "+session.AccessToken)
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return reconciliationPreflightUnavailable(w, r)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "login is required", httpserver.RequestID(r.Context()))
		return false
	}
	if response.StatusCode == http.StatusForbidden {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "financial reconciliation permission is required", httpserver.RequestID(r.Context()))
		return false
	}
	if response.StatusCode != http.StatusOK {
		return reconciliationPreflightUnavailable(w, r)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return reconciliationPreflightUnavailable(w, r)
	}
	var current struct {
		UserID              uuid.UUID `json:"user_id"`
		Status              string    `json:"status"`
		PlatformPermissions []string  `json:"platform_permissions"`
	}
	if json.Unmarshal(raw, &current) != nil || current.UserID == uuid.Nil || current.Status == "" || current.PlatformPermissions == nil {
		return reconciliationPreflightUnavailable(w, r)
	}
	if current.Status != "ACTIVE" {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "active account is required", httpserver.RequestID(r.Context()))
		return false
	}
	for _, permission := range current.PlatformPermissions {
		if permission == "platform.billing.reconciliation.run" {
			return true
		}
	}
	api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "financial reconciliation permission is required", httpserver.RequestID(r.Context()))
	return false
}
func reconciliationPreflightUnavailable(w http.ResponseWriter, r *http.Request) bool {
	api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "financial reconciliation authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
	return false
}
