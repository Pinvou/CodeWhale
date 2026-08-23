package token

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) resolveOrganizationBilling(w http.ResponseWriter, r *http.Request, permission string) (AuthorizationContext, uuid.UUID, OrganizationBillingClient, bool) {
	resolved, ok := s.resolveContext(w, r)
	organizationID, err := uuid.Parse(r.PathValue("organization_id"))
	client, supported := s.billing.(OrganizationBillingClient)
	if !ok {
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	if err != nil || !supported || resolved.ContextType != "ORGANIZATION" || resolved.OrganizationID == nil || *resolved.OrganizationID != organizationID || !hasPermission(resolved, permission) {
		s.writeError(w, r, ErrForbidden)
		return AuthorizationContext{}, uuid.Nil, nil, false
	}
	return resolved, organizationID, client, true
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
	payload, err := client.CreateOrganizationRechargeOrder(r.Context(), resolved.UserID, walletID, organizationID, body.ProductID, strings.ToUpper(body.Channel), *resolved.Mobile, commerceStatus, r.Header.Get("Idempotency-Key"))
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
	if err != nil || api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Reason) == "" {
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
