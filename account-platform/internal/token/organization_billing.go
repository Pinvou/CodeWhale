package token

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) resolveOrganizationBilling(w http.ResponseWriter, r *http.Request, permissions ...string) (AuthorizationContext, uuid.UUID, OrganizationBillingClient, bool) {
	resolved, ok := s.resolveContext(w, r)
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	client, supported := s.billing.(OrganizationBillingClient)
	if !ok {
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	authorized := false
	for _, permission := range permissions {
		authorized = authorized || hasPermission(resolved, permission)
	}
	if err != nil || organizationID == uuid.Nil || !supported || resolved.ContextType != "ORGANIZATION" || resolved.OrganizationID == nil || *resolved.OrganizationID != organizationID || !authorized {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	return resolved, organizationID, client, true
}

func (s *Service) organizationEntitlementProducts(w http.ResponseWriter, r *http.Request) {
	_, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase")
	if !ok {
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	payload, err := client.OrganizationProducts(r.Context(), organizationID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization products are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) organizationEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	_, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.allocate", "org.wallet.read", "org.entitlements.purchase")
	if !ok {
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	payload, err := client.OrganizationPackages(r.Context(), organizationID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) createOrganizationRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase")
	if !ok {
		return
	}
	// The community simulator remains usable without a Commerce identity.
	// Billing applies the provider's verified/synchronized-customer requirement.
	mobile := ""
	if resolved.MobileVerified && resolved.Mobile != nil {
		mobile = *resolved.Mobile
	}
	var body struct {
		ProductID string `json:"product_id"`
		Channel   string `json:"channel"`
	}
	if api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.ProductID) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization recharge request", httpserver.RequestID(r.Context()))
		return
	}
	if !validIdempotencyKey(r.Header.Get("Idempotency-Key")) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key must contain 16 to 128 UTF-8 characters", httpserver.RequestID(r.Context()))
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
	payload, err := client.CreateOrganizationRechargeOrder(r.Context(), resolved.UserID, walletID, organizationID, body.ProductID, strings.ToUpper(body.Channel), mobile, commerceStatus, r.Header.Get("Idempotency-Key"))
	if err != nil {
		if errors.Is(err, ErrCommerceCustomerNotReady) {
			api.WriteError(w, http.StatusConflict, "COMMERCE_CUSTOMER_NOT_READY", "the payment provider requires a ready Commerce Customer mapping", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusBadGateway, "RECHARGE_CREATE_FAILED", "the organization recharge order could not be created", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) listOrganizationQuotaGrants(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.read.self")
	if !ok {
		return
	}
	memberUserID := &resolved.UserID
	if hasPermission(resolved, "org.quota.read.all") {
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

func (s *Service) allocateOrganizationQuotaBatch(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, client, ok := s.resolveOrganizationBilling(w, r, "org.quota.allocate")
	if !ok {
		return
	}
	var body struct {
		Items json.RawMessage `json:"items"`
	}
	if api.DecodeJSON(w, r, &body) != nil || len(body.Items) == 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota allocation batch", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.AllocateOrganizationQuotaBatch(r.Context(), organizationID, resolved.UserID, body.Items, r.Header.Get("Idempotency-Key"))
	if err != nil {
		api.WriteError(w, http.StatusConflict, "QUOTA_ALLOCATION_FAILED", "the quota allocation batch could not be completed", httpserver.RequestID(r.Context()))
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
	if err != nil || api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Reason) == "" || utf8.RuneCountInString(body.Reason) > 500 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota revocation", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.RevokeOrganizationQuota(r.Context(), organizationID, grantID, resolved.UserID, body.Reason, r.Header.Get("Idempotency-Key"))
	if err != nil {
		api.WriteError(w, http.StatusConflict, "QUOTA_REVOKE_FAILED", "the quota grant cannot be revoked in its current state", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func hasPermission(resolved AuthorizationContext, permission string) bool {
	for _, candidate := range resolved.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}
