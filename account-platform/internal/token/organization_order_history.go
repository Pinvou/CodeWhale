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

// Keep the same requester boundary as the existing organization order detail
// handler. Organization membership never grants access to another payer's order.
func (s *Service) listOrganizationRechargeOrders(w http.ResponseWriter, r *http.Request) {
	resolved, organizationID, _, ok := s.resolveOrganizationBilling(w, r, "org.entitlements.purchase")
	if !ok {
		return
	}
	for key, values := range r.URL.Query() {
		if key != "cursor" || len(values) != 1 || len(values[0]) > 256 || strings.IndexFunc(values[0], func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-')
		}) >= 0 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid order history query", httpserver.RequestID(r.Context()))
			return
		}
	}
	client, supported := s.billing.(OrganizationOrderHistoryClient)
	if !supported {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization order history is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "ORGANIZATION", organizationID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization order history is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.OrganizationRechargeOrders(r.Context(), resolved.UserID, walletID, r.URL.Query().Get("cursor"))
	if err != nil {
		var failure *BillingHTTPError
		if errors.As(err, &failure) && failure.Status == http.StatusBadRequest {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid order history cursor", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "organization order history is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var page struct {
		RequesterID uuid.UUID         `json:"requester_id"`
		WalletID    uuid.UUID         `json:"wallet_id"`
		Items       []json.RawMessage `json:"items"`
		NextCursor  *string           `json:"next_cursor"`
	}
	if json.Unmarshal(payload, &page) != nil || page.RequesterID != resolved.UserID || page.WalletID != walletID || page.Items == nil || len(page.Items) > 50 || page.NextCursor != nil && (*page.NextCursor == "" || len(*page.NextCursor) > 256 || *page.NextCursor == r.URL.Query().Get("cursor")) {
		api.WriteError(w, http.StatusBadGateway, "BILLING_UNAVAILABLE", "invalid organization order history", httpserver.RequestID(r.Context()))
		return
	}
	seen := map[uuid.UUID]bool{}
	for _, raw := range page.Items {
		var item struct {
			ID uuid.UUID `json:"order_id"`
		}
		if json.Unmarshal(raw, &item) != nil || item.ID == uuid.Nil || seen[item.ID] {
			api.WriteError(w, http.StatusBadGateway, "BILLING_UNAVAILABLE", "invalid organization order history", httpserver.RequestID(r.Context()))
			return
		}
		seen[item.ID] = true
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"organization_id": organizationID, "items": page.Items, "next_cursor": page.NextCursor})
}
