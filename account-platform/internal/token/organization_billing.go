package token

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"strings"
)

func (s *Service) resolveOrganizationBilling(w http.ResponseWriter, r *http.Request, permission string) (AuthorizationContext, uuid.UUID, OrganizationBillingClient, bool) {
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	client, supported := s.billing.(OrganizationBillingClient)
	if err != nil || !supported {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	resolved, ok := s.resolveOrganizationPermission(w, r, organizationID, permission)
	if !ok {
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	return resolved, organizationID, client, true
}

func (s *Service) resolveOrganizationPermission(w http.ResponseWriter, r *http.Request, organizationID uuid.UUID, permission string) (AuthorizationContext, bool) {
	if s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	userID, authorizationVersion, err := s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, permission, organizationID)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return AuthorizationContext{}, false
	}
	resolved, ok := s.resolveContext(w, r)
	if !ok {
		return AuthorizationContext{}, false
	}
	if resolved.UserID != userID {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, false
	}
	resolved.AuthorizationVersion = authorizationVersion
	w.Header().Set("X-Authorization-Version", fmt.Sprint(authorizationVersion))
	return resolved, true
}

func (s *Service) getOrganizationBalance(w http.ResponseWriter, r *http.Request) {
	_, organizationID, _, ok := s.resolveOrganizationBilling(w, r, "org.wallet.read")
	if !ok {
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the organization balance is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.Balance(r.Context(), walletID)
	if err == nil {
		_, err = validateWalletOwnerFacts(payload, walletID, organizationID, "ORGANIZATION", true)
	}
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the organization balance is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getOrganizationEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, _, ok := s.resolveOrganizationBilling(w, r, "org.wallet.read")
	if !ok {
		return
	}
	client, supported := s.billing.(EntitlementPackageBillingClient)
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil || !supported {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.Packages(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getOrganizationRechargeProducts(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase"); !ok {
		return
	}
	payload, err := s.billing.Products(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization recharge products are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getOrganizationPaymentCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, _, _, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase"); !ok {
		return
	}
	client, ok := s.billing.(PaymentCapabilitiesBillingClient)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.PaymentCapabilities(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) createOrganizationRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase")
	if !ok {
		return
	}
	if !resolved.MobileVerified || resolved.Mobile == nil {
		api.WriteError(w, http.StatusConflict, "MOBILE_NOT_VERIFIED", "a verified mobile is required before recharge", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ProductID string `json:"product_id"`
		Channel   string `json:"channel"`
	}
	if api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.ProductID) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid recharge product and channel are required", httpserver.RequestID(r.Context()))
		return
	}
	commerceStatus := "NOT_CONFIGURED"
	if s.commerceCustomers != nil {
		if status, err := s.commerceCustomers.ResolveCommerceCustomer(r.Context(), r.Header.Get("Authorization")); err == nil {
			commerceStatus = status
		}
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the organization wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.CreateOrganizationRechargeOrder(paymentSourceContext(r), resolved.UserID, walletID, organizationID, body.ProductID, strings.ToUpper(body.Channel), *resolved.Mobile, commerceStatus, r.Header.Get("Idempotency-Key"))
	if err != nil {
		var limited *BillingHTTPError
		if errors.As(err, &limited) && limited.Status == http.StatusTooManyRequests {
			if limited.RetryAfter != "" {
				w.Header().Set("Retry-After", limited.RetryAfter)
			}
			api.WriteError(w, http.StatusTooManyRequests, "PAYMENT_RATE_LIMITED", "the payment action exceeds a rate limit", httpserver.RequestID(r.Context()))
			return
		}
		if errors.Is(err, ErrCommerceCustomerNotReady) {
			api.WriteError(w, http.StatusConflict, "COMMERCE_CUSTOMER_NOT_READY", "the payment provider requires a ready Commerce Customer mapping", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusBadGateway, "RECHARGE_CREATE_FAILED", "the organization recharge order could not be created", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getOrganizationRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, _, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization recharge order", httpserver.RequestID(r.Context()))
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the organization wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RechargeOrder(paymentSourceContext(r), orderID, resolved.UserID)
	if err != nil {
		var limited *BillingHTTPError
		if errors.As(err, &limited) && limited.Status == http.StatusTooManyRequests {
			if limited.RetryAfter != "" {
				w.Header().Set("Retry-After", limited.RetryAfter)
			}
			api.WriteError(w, http.StatusTooManyRequests, "PAYMENT_RATE_LIMITED", "the payment action exceeds a rate limit", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the organization recharge order was not found", httpserver.RequestID(r.Context()))
		return
	}
	var scope struct {
		BeneficiaryOrganizationID *uuid.UUID `json:"beneficiary_organization_id"`
		WalletID                  uuid.UUID  `json:"wallet_id"`
	}
	if json.Unmarshal(payload, &scope) != nil {
		api.WriteError(w, http.StatusBadGateway, "BILLING_UNAVAILABLE", "the organization recharge order is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if scope.BeneficiaryOrganizationID == nil || *scope.BeneficiaryOrganizationID != organizationID || scope.WalletID != walletID {
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the organization recharge order was not found", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) listOrganizationQuotaGrants(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.read.self")
	if !ok {
		return
	}
	memberUserID := &resolved.UserID
	allGranted, err := s.organizationPermissionAllowed(r, organizationID, "org.quota.read.all", resolved.UserID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if allGranted {
		if raw := strings.TrimSpace(r.URL.Query().Get("member_user_id")); raw == "" {
			memberUserID = nil
		} else if parsed, err := uuid.Parse(raw); err == nil {
			memberUserID = &parsed
		} else {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota lookup", httpserver.RequestID(r.Context()))
			return
		}
	}
	payload, err := client.OrganizationQuotaGrants(r.Context(), organizationID, memberUserID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "member quota grants are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) organizationPermissionAllowed(r *http.Request, organizationID uuid.UUID, permission string, expectedUserID uuid.UUID) (bool, error) {
	principal, ok := identity.FromContext(r.Context())
	if !ok || s.authorizer == nil {
		return false, errors.New("authorization is unavailable")
	}
	userID, _, err := s.authorizer.AuthorizedOrganizationUserID(r.Context(), principal, permission, organizationID)
	if errors.Is(err, authorization.ErrPermissionDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return userID == expectedUserID, nil
}

func (s *Service) allocateOrganizationQuotaBatch(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.allocate")
	if !ok {
		return
	}
	var body struct {
		Items json.RawMessage `json:"items"`
	}
	if api.DecodeJSON(w, r, &body) != nil || len(body.Items) == 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid quota allocation batch is required", httpserver.RequestID(r.Context()))
		return
	}
	var items []json.RawMessage
	if json.Unmarshal(body.Items, &items) != nil || items == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid quota allocation batch is required", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.AllocateOrganizationQuotaBatch(r.Context(), organizationID, resolved.UserID, body.Items, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeOrganizationQuotaFailure(w, r, err, "QUOTA_ALLOCATION_FAILED", "the quota allocation batch could not be completed")
		return
	}
	if !json.Valid(payload) {
		writeOrganizationQuotaFailure(w, r, errors.New("invalid quota receipt"), "QUOTA_ALLOCATION_FAILED", "the quota allocation batch could not be completed")
		return
	}
	writeRawNoStore(w, http.StatusMultiStatus, payload)
}

func (s *Service) revokeOrganizationQuotaGrant(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.revoke")
	if !ok {
		return
	}
	grantID, err := uuid.Parse(r.PathValue("grant_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota revocation", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.RevokeOrganizationQuota(r.Context(), organizationID, grantID, resolved.UserID, body.Reason, r.Header.Get("Idempotency-Key"))
	if err != nil {
		writeOrganizationQuotaFailure(w, r, err, "QUOTA_REVOKE_FAILED", "the quota grant cannot be revoked in its current state")
		return
	}
	if !json.Valid(payload) {
		writeOrganizationQuotaFailure(w, r, errors.New("invalid quota receipt"), "QUOTA_REVOKE_FAILED", "the quota grant cannot be revoked in its current state")
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

// A transport/credential/service failure or malformed success receipt does not
// prove that a quota write was rejected. Only a typed domain HTTP rejection can
// be relayed as such; keep the caller's key and the upstream details private.
func writeOrganizationQuotaFailure(w http.ResponseWriter, r *http.Request, err error, code, message string) {
	var failure *BillingHTTPError
	if errors.As(err, &failure) {
		switch failure.Status {
		case http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
			if failure.Status == http.StatusTooManyRequests {
				validSeconds := failure.RetryAfter != ""
				for _, digit := range failure.RetryAfter {
					if digit < '0' || digit > '9' {
						validSeconds = false
						break
					}
				}
				if validSeconds {
					w.Header().Set("Retry-After", failure.RetryAfter)
				}
			}
			api.WriteError(w, failure.Status, code, message, httpserver.RequestID(r.Context()))
			return
		}
	}
	api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the quota result is temporarily unavailable; retry with the same idempotency key", httpserver.RequestID(r.Context()))
}
