package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
)

var ErrCommerceCustomerNotReady = errors.New("commerce customer mapping is not ready")

type BillingHTTPError struct {
	Status     int
	Code       string
	RetryAfter string
}

func (e *BillingHTTPError) Error() string {
	return fmt.Sprintf("Billing HTTP %d (%s)", e.Status, e.Code)
}

type PaymentCapabilitiesBillingClient interface {
	PaymentCapabilities(context.Context) (json.RawMessage, error)
}

type EntitlementPackageBillingClient interface {
	Packages(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
}

func (c *HTTPBillingClient) Products(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/v1/recharge/products", nil, "")
}

func (c *HTTPBillingClient) PaymentCapabilities(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/v1/payment-capabilities", nil, "")
}

func (c *HTTPBillingClient) CreateRechargeOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, commerceCustomerStatus, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile, "commerce_customer_status": commerceCustomerStatus}, idempotencyKey)
}

func (c *HTTPBillingClient) RechargeOrders(ctx context.Context, requesterID, walletID uuid.UUID) (json.RawMessage, error) {
	query := "?requester_id=" + url.QueryEscape(requesterID.String()) + "&wallet_id=" + url.QueryEscape(walletID.String())
	return c.raw(ctx, http.MethodGet, "/internal/v1/recharge/orders"+query, nil, "")
}

func (c *HTTPBillingClient) RecentRechargeOrder(ctx context.Context, requesterID, walletID uuid.UUID) (json.RawMessage, error) {
	query := "?requester_id=" + url.QueryEscape(requesterID.String()) + "&wallet_id=" + url.QueryEscape(walletID.String())
	return c.raw(ctx, http.MethodGet, "/internal/v1/recharge/orders/recent"+query, nil, "")
}

func (c *HTTPBillingClient) RechargeOrder(ctx context.Context, orderID, requesterID uuid.UUID) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/internal/v1/recharge/orders/"+orderID.String()+"?requester_id="+url.QueryEscape(requesterID.String()), nil, "")
}

func (c *HTTPBillingClient) CloseRechargeOrder(ctx context.Context, orderID, requesterID uuid.UUID) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders/"+orderID.String()+"/close", map[string]any{"requester_id": requesterID}, "")
}

func (c *HTTPBillingClient) AddLatePaymentClaim(ctx context.Context, orderID, requesterID uuid.UUID, statement string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders/"+orderID.String()+"/payment-claims", map[string]any{"requester_id": requesterID, "statement": statement}, "")
}

func (c *HTTPBillingClient) Packages(ctx context.Context, userID, walletID uuid.UUID) (json.RawMessage, error) {
	query := "?user_id=" + url.QueryEscape(userID.String()) + "&wallet_id=" + url.QueryEscape(walletID.String())
	return c.raw(ctx, http.MethodGet, "/internal/v1/entitlement-packages"+query, nil, "")
}

type paymentSourceContextKey struct{}

func paymentSourceContext(r *http.Request) context.Context {
	source := "source:unknown"
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		if candidate := strings.TrimSpace(forwarded[index]); net.ParseIP(candidate) != nil {
			source = candidate
			break
		}
	}
	if source == "source:unknown" {
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			source = host
		}
	}
	return context.WithValue(r.Context(), paymentSourceContextKey{}, source)
}
