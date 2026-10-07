package token

import (
	"context"

	"errors"

	"net/http"

	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
)

type CommerceCustomerResolver interface {
	ResolveCommerceCustomer(context.Context, string) (string, error)
}

func (s *Service) listRechargeOrders(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "recharge orders are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RechargeOrders(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "recharge orders are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) rechargeProducts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolvePersonalBilling(w, r, "personal.billing.read"); !ok {
		return
	}
	payload, err := s.billing.Products(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the recharge catalog is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) paymentCapabilities(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.resolvePersonalBilling(w, r, "personal.billing.read"); !ok {
		return
	}
	client, ok := s.billing.(PaymentCapabilitiesBillingClient)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.PaymentCapabilities(r.Context())
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "payment capabilities are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) createRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.create")
	if !ok {
		return
	}
	commerceStatus := "NOT_CONFIGURED"
	if s.commerceCustomers != nil {
		status, err := s.commerceCustomers.ResolveCommerceCustomer(r.Context(), r.Header.Get("Authorization"))
		if err == nil {
			commerceStatus = status
		}
	}
	if !resolved.MobileVerified || resolved.Mobile == nil {
		api.WriteError(w, http.StatusConflict, "MOBILE_NOT_VERIFIED", "a verified mobile is required before recharge", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ProductID string `json:"product_id"`
		Channel   string `json:"channel"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid recharge product and channel are required", httpserver.RequestID(r.Context()))
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.CreateRechargeOrder(paymentSourceContext(r), resolved.UserID, walletID, body.ProductID, strings.ToUpper(body.Channel), *resolved.Mobile, commerceStatus, r.Header.Get("Idempotency-Key"))
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
		api.WriteError(w, http.StatusBadGateway, "RECHARGE_CREATE_FAILED", "the recharge order could not be created", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
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
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the recharge order was not found", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) getRecentRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.activeWallet(r.Context(), resolved)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the active wallet is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.RecentRechargeOrder(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "the recent recharge order is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) closeRechargeOrder(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.manage")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.CloseRechargeOrder(r.Context(), orderID, resolved.UserID)
	if err != nil {
		api.WriteError(w, http.StatusNotFound, "RECHARGE_ORDER_NOT_FOUND", "the recharge order was not found or cannot be closed", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (s *Service) addLatePaymentClaim(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.recharge.manage")
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		Statement string `json:"statement"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || len(strings.TrimSpace(body.Statement)) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid order and payment statement are required", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := s.billing.AddLatePaymentClaim(r.Context(), orderID, resolved.UserID, body.Statement)
	if err != nil {
		api.WriteError(w, http.StatusConflict, "PAYMENT_CLAIM_REJECTED", "the payment claim cannot be added to this order", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusCreated, payload)
}

func (s *Service) getEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	resolved, ok := s.resolvePersonalBilling(w, r, "personal.billing.read")
	if !ok {
		return
	}
	walletID, err := s.wallets.Ensure(r.Context(), "PERSONAL", resolved.UserID)
	client, supported := s.billing.(EntitlementPackageBillingClient)
	if err != nil || !supported {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	payload, err := client.Packages(r.Context(), resolved.UserID, walletID)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "BILLING_UNAVAILABLE", "entitlement packages are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	writeRawNoStore(w, http.StatusOK, payload)
}

func (r *HTTPContextResolver) ResolveCommerceCustomer(ctx context.Context, authorization string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL.String()+"/v1/me/commerce-customer-mapping", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", authorization)
	response, err := r.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", ErrForbidden
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeAccountAuthority(response.Body, 64<<10, &body); err != nil {
		return "", err
	}
	return body.Status, nil
}
