package token

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func (s *Service) reclaimAccountClosedGrant(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, baseClient, ok := s.resolveOrganizationBilling(w, r, "organization.grant.reclaim_closed")
	if !ok {
		return
	}
	client, supported := baseClient.(AccountClosedGrantBillingClient)
	if !supported {
		s.writeError(w, r, ErrForbidden)
		return
	}
	grantID, err := uuid.Parse(r.PathValue("grant_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if err != nil || api.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Reason) == "" || len(key) < 16 || len(key) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid account-closed grant reclaim", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.ReclaimAccountClosedGrant(r.Context(), organizationID, grantID, resolved.UserID, body.Reason, key)
	if err != nil {
		api.WriteError(w, http.StatusConflict, "ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED", "the account-closed grant cannot be reclaimed in its current state", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) reclaimAccountClosedGrantsBatch(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, baseClient, ok := s.resolveOrganizationBilling(w, r, "organization.grant.reclaim_closed")
	if !ok {
		return
	}
	client, supported := baseClient.(AccountClosedGrantBillingClient)
	if !supported {
		s.writeError(w, r, ErrForbidden)
		return
	}
	var body struct {
		GrantIDs []uuid.UUID `json:"grant_ids"`
		Reason   string      `json:"reason"`
	}
	batchKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if api.DecodeJSON(w, r, &body) != nil || len(body.GrantIDs) == 0 || len(body.GrantIDs) > 200 || strings.TrimSpace(body.Reason) == "" || len(batchKey) < 16 || len(batchKey) > 128 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid account-closed grant reclaim batch", httpserver.RequestID(r.Context()))
		return
	}
	type result struct {
		GrantID uuid.UUID       `json:"grant_id"`
		Status  string          `json:"status"`
		Result  json.RawMessage `json:"result,omitempty"`
		Code    string          `json:"code,omitempty"`
	}
	items := make([]result, 0, len(body.GrantIDs))
	for index, grantID := range body.GrantIDs {
		digest := sha256.Sum256([]byte(fmt.Sprintf("account-closed-reclaim-batch-v1\n%s\n%d\n%s", batchKey, index, grantID)))
		payload, err := client.ReclaimAccountClosedGrant(r.Context(), organizationID, grantID, resolved.UserID, body.Reason, hex.EncodeToString(digest[:]))
		item := result{GrantID: grantID, Status: "SUCCEEDED", Result: payload}
		if err != nil {
			item.Status, item.Result, item.Code = "FAILED", nil, "ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED"
		}
		items = append(items, item)
	}
	api.WriteJSON(w, http.StatusMultiStatus, map[string]any{"items": items})
}

func hasPermission(resolved AuthorizationContext, permission string) bool {
	for _, candidate := range resolved.Permissions {
		if candidate == permission {
			return true
		}
	}
	return false
}
