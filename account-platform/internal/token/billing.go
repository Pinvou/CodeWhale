package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
)

type PriceVersion struct {
	ID                    uuid.UUID `json:"price_version_id"`
	Model                 string    `json:"model"`
	InputPerMillion       int64     `json:"input_per_million"`
	OutputPerMillion      int64     `json:"output_per_million"`
	CachedInputPerMillion int64     `json:"cached_input_per_million"`
}

type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

type BillingHold struct {
	ID             uuid.UUID `json:"hold_id"`
	UsageRequestID uuid.UUID `json:"usage_request_id"`
	Status         string    `json:"status"`
}

type BillingClient interface {
	Price(context.Context, string) (PriceVersion, error)
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error)
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error
	Release(context.Context, uuid.UUID, uuid.UUID, string) error
	Products(context.Context) (json.RawMessage, error)
	CreateRechargeOrder(context.Context, uuid.UUID, uuid.UUID, string, string, string, string) (json.RawMessage, error)
	RecentRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	CloseRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	Balance(context.Context, uuid.UUID) (json.RawMessage, error)
}

type EntitlementPackageBillingClient interface {
	Packages(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
}

func (c *HTTPBillingClient) Products(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/v1/recharge/products", nil, "")
}

func (c *HTTPBillingClient) CreateRechargeOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile}, idempotencyKey)
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

func (c *HTTPBillingClient) Balance(ctx context.Context, walletID uuid.UUID) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/internal/v1/wallets/"+walletID.String()+"/balance", nil, "")
}

func (c *HTTPBillingClient) Packages(ctx context.Context, userID, walletID uuid.UUID) (json.RawMessage, error) {
	query := "?user_id=" + url.QueryEscape(userID.String()) + "&wallet_id=" + url.QueryEscape(walletID.String())
	return c.raw(ctx, http.MethodGet, "/internal/v1/entitlement-packages"+query, nil, "")
}

func (c *HTTPBillingClient) raw(ctx context.Context, method, path string, input any, idempotencyKey string) (json.RawMessage, error) {
	var reader io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+path, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Billing HTTP %d: %s", response.StatusCode, payload)
	}
	return json.RawMessage(payload), nil
}

func Cost(price PriceVersion, usage Usage) (int64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 || usage.CachedInputTokens > usage.InputTokens {
		return 0, ErrForbidden
	}
	regularInput := usage.InputTokens - usage.CachedInputTokens
	input, err := tokenCost(regularInput, price.InputPerMillion)
	if err != nil {
		return 0, err
	}
	cached, err := tokenCost(usage.CachedInputTokens, price.CachedInputPerMillion)
	if err != nil || input > math.MaxInt64-cached {
		return 0, ErrForbidden
	}
	output, err := tokenCost(usage.OutputTokens, price.OutputPerMillion)
	if err != nil || input+cached > math.MaxInt64-output {
		return 0, ErrForbidden
	}
	return input + cached + output, nil
}

func tokenCost(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 || (rate != 0 && tokens > (math.MaxInt64-999_999)/rate) {
		return 0, ErrForbidden
	}
	return (tokens*rate + 999_999) / 1_000_000, nil
}

type HTTPBillingClient struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPBillingClient(rawURL, serviceToken string) (*HTTPBillingClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid Billing client configuration")
	}
	return &HTTPBillingClient{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(10 * time.Second)}, nil
}

func (c *HTTPBillingClient) Price(ctx context.Context, model string) (PriceVersion, error) {
	var price PriceVersion
	err := c.call(ctx, http.MethodGet, "/internal/v1/pricing/current?model="+url.QueryEscape(model), nil, &price)
	return price, err
}

func (c *HTTPBillingClient) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	var price PriceVersion
	err := c.call(ctx, http.MethodGet, "/internal/v1/pricing/"+priceID.String(), nil, &price)
	return price, err
}

func (c *HTTPBillingClient) Reserve(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900}, &hold)
	return hold, err
}

func (c *HTTPBillingClient) Commit(ctx context.Context, usageID, holdID uuid.UUID, actual int64, usage Usage) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/commit", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "actual_microcredits": actual, "usage_snapshot": usage}, nil)
}

func (c *HTTPBillingClient) Release(ctx context.Context, usageID, holdID uuid.UUID, reason string) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/release", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "reason": reason}, nil)
}

func (c *HTTPBillingClient) call(ctx context.Context, method, path string, input, output any) error {
	var reader io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.serviceToken)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("Billing HTTP %d: %s", response.StatusCode, data)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(output)
}

func MaximumUsage(maxInputTokens, maxOutputTokens int64) Usage {
	return Usage{InputTokens: maxInputTokens, OutputTokens: maxOutputTokens}
}
