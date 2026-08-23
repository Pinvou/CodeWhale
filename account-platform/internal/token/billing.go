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
	ID                    uuid.UUID           `json:"price_version_id"`
	Model                 string              `json:"model"`
	InputPerMillion       int64               `json:"input_per_million"`
	OutputPerMillion      int64               `json:"output_per_million"`
	CachedInputPerMillion int64               `json:"cached_input_per_million"`
	ScopeType             string              `json:"scope_type"`
	ProtocolCode          string              `json:"protocol_code"`
	FormulaVersionID      uuid.UUID           `json:"formula_version_id"`
	FormulaSnapshot       json.RawMessage     `json:"formula_snapshot"`
	CreditUnit            string              `json:"credit_unit"`
	EffectiveFrom         time.Time           `json:"effective_from"`
	EffectiveTo           *time.Time          `json:"effective_to,omitempty"`
	ResolvedAt            time.Time           `json:"resolved_at"`
	Units                 []ResolvedUnitPrice `json:"units"`
}

type ResolvedUnitPrice struct {
	UnitCode             string    `json:"unit_code"`
	UnitDefinitionID     uuid.UUID `json:"unit_definition_id"`
	UnitVersion          int64     `json:"unit_version"`
	MinimumPrecision     int64     `json:"minimum_precision"`
	PriceDenominator     int64     `json:"price_denominator"`
	RoundingRule         string    `json:"rounding_rule"`
	ResolvedPrice        int64     `json:"resolved_price"`
	ConfigurationType    string    `json:"configuration_type"`
	PriceSourceScope     string    `json:"price_source_scope"`
	SourcePriceVersionID uuid.UUID `json:"source_price_version_id"`
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

var ErrCommerceCustomerNotReady = errors.New("commerce customer mapping is not ready")

type BillingClient interface {
	Price(context.Context, string) (PriceVersion, error)
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error)
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error
	Release(context.Context, uuid.UUID, uuid.UUID, string) error
	Products(context.Context) (json.RawMessage, error)
	CreateRechargeOrder(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	RechargeOrders(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RecentRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	CloseRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	Balance(context.Context, uuid.UUID) (json.RawMessage, error)
}

type HoldPricingContext struct {
	ModelID          string
	ProtocolCode     string
	FormulaVersionID uuid.UUID
	PricingSnapshot  json.RawMessage
	RequestSnapshot  json.RawMessage
}

type SnapshotBillingClient interface {
	ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, HoldPricingContext) (BillingHold, error)
	Extend(context.Context, uuid.UUID, uuid.UUID, int64) error
}

type FormulaBindingClient interface {
	RegisterFormulaBinding(context.Context, uuid.UUID, string, []uuid.UUID, json.RawMessage, time.Time) error
}

type SnapshotOrganizationBillingClient interface {
	ReserveMemberQuotaPriced(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, uuid.UUID, HoldPricingContext) (BillingHold, error)
}

type EntitlementPackageBillingClient interface {
	Packages(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
}

type OrganizationBillingClient interface {
	CreateOrganizationRechargeOrder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	OrganizationQuotaGrants(context.Context, uuid.UUID, *uuid.UUID) (json.RawMessage, error)
	AllocateOrganizationQuotaBatch(context.Context, uuid.UUID, uuid.UUID, json.RawMessage, string) (json.RawMessage, error)
	RevokeOrganizationQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (json.RawMessage, error)
	ReserveMemberQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, uuid.UUID) (BillingHold, error)
}

func (c *HTTPBillingClient) Products(ctx context.Context) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/v1/recharge/products", nil, "")
}

func (c *HTTPBillingClient) CreateRechargeOrder(ctx context.Context, requesterID, walletID uuid.UUID, productID, channel, mobile, commerceCustomerStatus, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile, "commerce_customer_status": commerceCustomerStatus}, idempotencyKey)
}

func (c *HTTPBillingClient) CreateOrganizationRechargeOrder(ctx context.Context, requesterID, walletID, organizationID uuid.UUID, productID, channel, mobile, commerceCustomerStatus, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "beneficiary_organization_id": organizationID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile, "commerce_customer_status": commerceCustomerStatus}, idempotencyKey)
}

func (c *HTTPBillingClient) OrganizationQuotaGrants(ctx context.Context, organizationID uuid.UUID, memberUserID *uuid.UUID) (json.RawMessage, error) {
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants"
	if memberUserID != nil {
		path += "?member_user_id=" + url.QueryEscape(memberUserID.String())
	}
	return c.raw(ctx, http.MethodGet, path, nil, "")
}

func (c *HTTPBillingClient) AllocateOrganizationQuotaBatch(ctx context.Context, organizationID, actorUserID uuid.UUID, items json.RawMessage, idempotencyKey string) (json.RawMessage, error) {
	var decoded []map[string]any
	if err := json.Unmarshal(items, &decoded); err != nil {
		return nil, err
	}
	return c.raw(ctx, http.MethodPost, "/internal/v1/organizations/"+organizationID.String()+"/quota-grants:batch", map[string]any{"created_by": actorUserID, "items": decoded}, idempotencyKey)
}

func (c *HTTPBillingClient) RevokeOrganizationQuota(ctx context.Context, organizationID, grantID, actorUserID uuid.UUID, reason, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/organizations/"+organizationID.String()+"/quota-grants/"+grantID.String()+"/revoke", map[string]any{"actor_user_id": actorUserID, "reason": reason}, idempotencyKey)
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
		var problem struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(payload, &problem) == nil && problem.Code == "COMMERCE_CUSTOMER_NOT_READY" {
			return nil, ErrCommerceCustomerNotReady
		}
		return nil, fmt.Errorf("Billing HTTP %d: %s", response.StatusCode, payload)
	}
	return json.RawMessage(payload), nil
}

func Cost(price PriceVersion, usage Usage) (int64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return 0, ErrForbidden
	}
	if len(price.Units) == 0 {
		input, err := tokenCost(usage.InputTokens, price.InputPerMillion)
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
	quantities := map[string]int64{"INPUT_TOKEN": usage.InputTokens, "CACHED_INPUT_TOKEN": usage.CachedInputTokens, "OUTPUT_TOKEN": usage.OutputTokens}
	var total int64
	for _, unit := range price.Units {
		quantity, ok := quantities[unit.UnitCode]
		if !ok || unit.MinimumPrecision <= 0 || unit.PriceDenominator <= 0 || unit.ResolvedPrice < 0 {
			return 0, ErrForbidden
		}
		component, err := unitCost(quantity, unit)
		if err != nil || total > math.MaxInt64-component {
			return 0, ErrForbidden
		}
		total += component
		delete(quantities, unit.UnitCode)
	}
	if len(quantities) != 0 {
		return 0, ErrForbidden
	}
	return total, nil
}

func unitCost(quantity int64, unit ResolvedUnitPrice) (int64, error) {
	if quantity < 0 || unit.ResolvedPrice < 0 || unit.PriceDenominator <= 0 || unit.MinimumPrecision <= 0 || quantity > math.MaxInt64/unit.MinimumPrecision {
		return 0, ErrForbidden
	}
	precisionUnits := quantity / unit.MinimumPrecision
	if quantity%unit.MinimumPrecision != 0 {
		precisionUnits++
	}
	if unit.ResolvedPrice != 0 && precisionUnits > math.MaxInt64/unit.ResolvedPrice {
		return 0, ErrForbidden
	}
	numerator := precisionUnits * unit.ResolvedPrice
	switch unit.RoundingRule {
	case "CEILING":
		if numerator > math.MaxInt64-(unit.PriceDenominator-1) {
			return 0, ErrForbidden
		}
		return (numerator + unit.PriceDenominator - 1) / unit.PriceDenominator, nil
	case "FLOOR":
		return numerator / unit.PriceDenominator, nil
	case "HALF_UP":
		if numerator > math.MaxInt64-unit.PriceDenominator/2 {
			return 0, ErrForbidden
		}
		return (numerator + unit.PriceDenominator/2) / unit.PriceDenominator, nil
	default:
		return 0, ErrForbidden
	}
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

func (c *HTTPBillingClient) ReservePriced(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", holdReservationBody(usageID, walletID, maximum, priceID, pricing), &hold)
	return hold, err
}

func holdReservationBody(usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) map[string]any {
	return map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900, "model_id": pricing.ModelID, "protocol_code": pricing.ProtocolCode, "formula_version_id": pricing.FormulaVersionID, "pricing_snapshot": pricing.PricingSnapshot, "request_snapshot": pricing.RequestSnapshot}
}

func (c *HTTPBillingClient) ReserveMemberQuota(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "organization_id": organizationID, "member_user_id": memberUserID, "membership_version": membershipVersion, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900}, &hold)
	return hold, err
}

func (c *HTTPBillingClient) ReserveMemberQuotaPriced(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	var hold BillingHold
	body := holdReservationBody(usageID, walletID, maximum, priceID, pricing)
	body["organization_id"], body["member_user_id"], body["membership_version"] = organizationID, memberUserID, membershipVersion
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", body, &hold)
	return hold, err
}

func (c *HTTPBillingClient) Extend(ctx context.Context, usageID, holdID uuid.UUID, additional int64) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/extend", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "lease_seconds": 900, "additional_microcredits": additional}, nil)
}

func (c *HTTPBillingClient) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage, publishedAt time.Time) error {
	return c.call(ctx, http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formulaID.String(), map[string]any{"protocol_code": protocol, "required_unit_definition_ids": required, "formula_snapshot": snapshot, "published_at": publishedAt}, nil)
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
