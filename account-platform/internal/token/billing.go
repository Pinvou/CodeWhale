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
	ID                 uuid.UUID  `json:"hold_id"`
	UsageRequestID     uuid.UUID  `json:"usage_request_id"`
	Status             string     `json:"status"`
	WalletID           uuid.UUID  `json:"wallet_id"`
	PriceVersionID     *uuid.UUID `json:"price_version_id,omitempty"`
	MaxMicrocredits    *int64     `json:"max_microcredits"`
	ActualMicrocredits *int64     `json:"actual_microcredits,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at"`
}

var ErrCommerceCustomerNotReady = errors.New("commerce customer mapping is not ready")

type BillingClient interface {
	Price(context.Context, string) (PriceVersion, error)
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error)
	Commit(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, Usage) error
	Release(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) error
	Products(context.Context) (json.RawMessage, error)
	CreateRechargeOrder(context.Context, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	RechargeOrders(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RecentRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	RechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	CloseRechargeOrder(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	AddLatePaymentClaim(context.Context, uuid.UUID, uuid.UUID, string) (json.RawMessage, error)
	Balance(context.Context, uuid.UUID) (json.RawMessage, error)
}

type PaymentCapabilitiesBillingClient interface {
	PaymentCapabilities(context.Context) (json.RawMessage, error)
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
	Extend(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64) error
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

type OrganizationProductBillingClient interface {
	ProductsForOrganization(context.Context, uuid.UUID) (json.RawMessage, error)
}

func (c *HTTPBillingClient) ProductsForOrganization(ctx context.Context, walletID uuid.UUID) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/internal/v1/recharge/products?wallet_id="+url.QueryEscape(walletID.String()), nil, "")
}

type OrganizationBillingClient interface {
	OrganizationProducts(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	OrganizationPackages(context.Context, uuid.UUID, uuid.UUID) (json.RawMessage, error)
	CreateOrganizationRechargeOrder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	OrganizationQuotaGrants(context.Context, uuid.UUID, *uuid.UUID) (json.RawMessage, error)
	AllocateOrganizationQuotaBatch(context.Context, uuid.UUID, uuid.UUID, json.RawMessage, string) (json.RawMessage, error)
	RevokeOrganizationQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (json.RawMessage, error)
	ReserveMemberQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, uuid.UUID) (BillingHold, error)
}

func (c *HTTPBillingClient) OrganizationProducts(ctx context.Context, organizationID, walletID uuid.UUID) (json.RawMessage, error) {
	return c.organizationRead(ctx, "/internal/v1/organizations/"+organizationID.String()+"/recharge/products?wallet_id="+url.QueryEscape(walletID.String()))
}

func (c *HTTPBillingClient) OrganizationPackages(ctx context.Context, organizationID, walletID uuid.UUID) (json.RawMessage, error) {
	return c.organizationRead(ctx, "/internal/v1/organizations/"+organizationID.String()+"/entitlement-packages?wallet_id="+url.QueryEscape(walletID.String()))
}

func (c *HTTPBillingClient) organizationRead(ctx context.Context, path string) (json.RawMessage, error) {
	return c.organizationResponse(ctx, http.MethodGet, path, nil, "", true, http.StatusOK)
}

func (c *HTTPBillingClient) organizationResponse(ctx context.Context, method, path string, input any, key string, requireItems bool, statuses ...int) (json.RawMessage, error) {
	response, err := c.rawPolicy(ctx, method, path, input, key, statuses)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(response, &object); err != nil || object == nil {
		return nil, errors.New("Billing organization response is invalid")
	}
	if requireItems {
		var items []json.RawMessage
		if json.Unmarshal(object["items"], &items) != nil || items == nil {
			return nil, errors.New("Billing organization response has no valid items")
		}
	}
	return response, nil
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

func (c *HTTPBillingClient) CreateOrganizationRechargeOrder(ctx context.Context, requesterID, walletID, organizationID uuid.UUID, productID, channel, mobile, commerceCustomerStatus, idempotencyKey string) (json.RawMessage, error) {
	return c.organizationResponse(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "beneficiary_organization_id": organizationID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile, "commerce_customer_status": commerceCustomerStatus}, idempotencyKey, false, http.StatusOK, http.StatusCreated)
}

func (c *HTTPBillingClient) OrganizationQuotaGrants(ctx context.Context, organizationID uuid.UUID, memberUserID *uuid.UUID) (json.RawMessage, error) {
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants"
	if memberUserID != nil {
		path += "?member_user_id=" + url.QueryEscape(memberUserID.String())
	}
	return c.organizationResponse(ctx, http.MethodGet, path, nil, "", true, http.StatusOK)
}

func (c *HTTPBillingClient) AllocateOrganizationQuotaBatch(ctx context.Context, organizationID, actorUserID uuid.UUID, items json.RawMessage, idempotencyKey string) (json.RawMessage, error) {
	var decoded []json.RawMessage
	if err := json.Unmarshal(items, &decoded); err != nil {
		return nil, err
	}
	return c.organizationResponse(ctx, http.MethodPost, "/internal/v1/organizations/"+organizationID.String()+"/quota-grants:batch", map[string]any{"created_by": actorUserID, "items": decoded}, idempotencyKey, true, http.StatusMultiStatus)
}

func (c *HTTPBillingClient) RevokeOrganizationQuota(ctx context.Context, organizationID, grantID, actorUserID uuid.UUID, reason, idempotencyKey string) (json.RawMessage, error) {
	return c.organizationResponse(ctx, http.MethodPost, "/internal/v1/organizations/"+organizationID.String()+"/quota-grants/"+grantID.String()+"/revoke", map[string]any{"actor_user_id": actorUserID, "reason": reason}, idempotencyKey, false, http.StatusOK)
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

func (c *HTTPBillingClient) Balance(ctx context.Context, walletID uuid.UUID) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodGet, "/internal/v1/wallets/"+walletID.String()+"/balance", nil, "")
}

func (c *HTTPBillingClient) Packages(ctx context.Context, userID, walletID uuid.UUID) (json.RawMessage, error) {
	query := "?user_id=" + url.QueryEscape(userID.String()) + "&wallet_id=" + url.QueryEscape(walletID.String())
	return c.raw(ctx, http.MethodGet, "/internal/v1/entitlement-packages"+query, nil, "")
}

func (c *HTTPBillingClient) raw(ctx context.Context, method, path string, input any, idempotencyKey string) (json.RawMessage, error) {
	return c.rawPolicy(ctx, method, path, input, idempotencyKey, nil)
}

func (c *HTTPBillingClient) rawPolicy(ctx context.Context, method, path string, input any, idempotencyKey string, statuses []int) (json.RawMessage, error) {
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
	client := *c.client
	if len(statuses) > 0 {
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if readErr != nil {
		return nil, readErr
	}
	if len(payload) > 2<<20 {
		return nil, errors.New("Billing response exceeds the allowed size")
	}
	accepted := response.StatusCode >= 200 && response.StatusCode < 300
	if len(statuses) > 0 {
		accepted = false
		for _, status := range statuses {
			if response.StatusCode == status {
				accepted = true
			}
		}
	}
	if !accepted {
		var problem struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(payload, &problem) == nil && problem.Code == "COMMERCE_CUSTOMER_NOT_READY" {
			return nil, ErrCommerceCustomerNotReady
		}
		if len(statuses) > 0 {
			return nil, fmt.Errorf("Billing HTTP %d", response.StatusCode)
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
	if quantity < 0 || unit.ResolvedPrice < 0 || unit.PriceDenominator <= 0 || unit.MinimumPrecision <= 0 {
		return 0, ErrForbidden
	}
	// Quantity is the canonical unit amount, not a count of precision ticks.
	// The versioned precision remains metadata; the declared price denominator
	// determines scaling for both reservation and actual settlement.
	if unit.ResolvedPrice != 0 && quantity > math.MaxInt64/unit.ResolvedPrice {
		return 0, ErrForbidden
	}
	numerator := quantity * unit.ResolvedPrice
	switch unit.RoundingRule {
	case "CEILING":
		quotient := numerator / unit.PriceDenominator
		if numerator%unit.PriceDenominator != 0 {
			quotient++
		}
		return quotient, nil
	case "FLOOR":
		return numerator / unit.PriceDenominator, nil
	case "HALF_UP":
		quotient := numerator / unit.PriceDenominator
		if numerator%unit.PriceDenominator >= unit.PriceDenominator/2+unit.PriceDenominator%2 {
			quotient++
		}
		return quotient, nil
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

// MaximumCost bounds both cached and uncached input splits without changing
// Cost's existing component-by-component ceiling semantics.
func MaximumCost(price PriceVersion, inputTokens, outputTokens int64) (int64, error) {
	if price.InputPerMillion < 0 || price.CachedInputPerMillion < 0 || price.OutputPerMillion < 0 {
		return 0, ErrForbidden
	}
	input, err := tokenCost(inputTokens, max(price.InputPerMillion, price.CachedInputPerMillion))
	if err != nil {
		return 0, err
	}
	// ceil(a)+ceil(b) <= ceil(a+b)+1; two fractional input components
	// can add one more microcredit than pricing all input as a single group.
	if inputTokens >= 2 && price.InputPerMillion%1_000_000 != 0 && price.CachedInputPerMillion%1_000_000 != 0 {
		if input == math.MaxInt64 {
			return 0, ErrForbidden
		}
		input++
	}
	output, err := tokenCost(outputTokens, price.OutputPerMillion)
	if err != nil || input > math.MaxInt64-output {
		return 0, ErrForbidden
	}
	return input + output, nil
}

type HTTPBillingClient struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPBillingClient(rawURL, serviceToken string) (*HTTPBillingClient, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("invalid Billing client configuration")
	}
	client := observability.NewHTTPClient(10 * time.Second)
	client.CheckRedirect = rejectProviderRedirect
	return &HTTPBillingClient{baseURL: parsed, serviceToken: serviceToken, client: client}, nil
}

func (c *HTTPBillingClient) Price(ctx context.Context, model string) (PriceVersion, error) {
	price, err := c.readPrice(ctx, "/internal/v1/pricing/current?model="+url.QueryEscape(model))
	if err == nil && price.Model != model {
		err = errors.New("Billing price does not match requested model")
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return price, nil
}

func (c *HTTPBillingClient) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	price, err := c.readPrice(ctx, "/internal/v1/pricing/"+priceID.String())
	if err == nil && price.ID != priceID {
		err = errors.New("Billing price does not match requested version")
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return price, nil
}

func (c *HTTPBillingClient) readPrice(ctx context.Context, path string) (PriceVersion, error) {
	var wire struct {
		PriceVersion
		ID           uuid.UUID `json:"price_version_id"`
		Model        string    `json:"model"`
		Input        *int64    `json:"input_per_million"`
		Output       *int64    `json:"output_per_million"`
		Cached       *int64    `json:"cached_input_per_million"`
		UnitPresence []struct {
			ResolvedUnitPrice
			Amount *int64 `json:"resolved_price"`
		} `json:"units"`
	}
	if err := c.call(ctx, http.MethodGet, path, nil, &wire); err != nil {
		return PriceVersion{}, err
	}
	if wire.ID == uuid.Nil || strings.TrimSpace(wire.Model) == "" || wire.Input == nil || wire.Output == nil || wire.Cached == nil || *wire.Input < 0 || *wire.Output < 0 || *wire.Cached < 0 {
		return PriceVersion{}, errors.New("Billing price response is incomplete or invalid")
	}
	price := wire.PriceVersion
	price.ID, price.Model, price.InputPerMillion, price.OutputPerMillion, price.CachedInputPerMillion = wire.ID, wire.Model, *wire.Input, *wire.Output, *wire.Cached
	price.Units = make([]ResolvedUnitPrice, len(wire.UnitPresence))
	for index, unit := range wire.UnitPresence {
		if unit.Amount == nil {
			return PriceVersion{}, errors.New("Billing unit price response is incomplete")
		}
		price.Units[index] = unit.ResolvedUnitPrice
		price.Units[index].ResolvedPrice = *unit.Amount
	}
	if len(price.Units) > 0 {
		if err := ValidatePriceFormula(price); err != nil {
			return PriceVersion{}, err
		}
	}
	return price, nil
}

func (c *HTTPBillingClient) Reserve(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900}, &hold)
	if err != nil {
		return BillingHold{}, err
	}
	if !hold.matches(usageID, walletID) || hold.Status != "ACTIVE" || hold.PriceVersionID == nil || *hold.PriceVersionID != priceID || *hold.MaxMicrocredits != maximum || hold.ActualMicrocredits != nil {
		return BillingHold{}, errors.New("Billing reservation facts do not match")
	}
	return hold, nil
}

func (c *HTTPBillingClient) ReservePriced(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", holdReservationBody(usageID, walletID, maximum, priceID, pricing), &hold)
	if err != nil {
		return BillingHold{}, err
	}
	if !hold.matches(usageID, walletID) || hold.Status != "ACTIVE" || hold.PriceVersionID == nil || *hold.PriceVersionID != priceID || *hold.MaxMicrocredits != maximum || hold.ActualMicrocredits != nil {
		return BillingHold{}, errors.New("Billing reservation facts do not match")
	}
	return hold, nil
}

func holdReservationBody(usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) map[string]any {
	return map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900, "model_id": pricing.ModelID, "protocol_code": pricing.ProtocolCode, "formula_version_id": pricing.FormulaVersionID, "pricing_snapshot": pricing.PricingSnapshot, "request_snapshot": pricing.RequestSnapshot}
}

func (c *HTTPBillingClient) ReserveMemberQuota(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "organization_id": organizationID, "member_user_id": memberUserID, "membership_version": membershipVersion, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900}, &hold)
	if err != nil {
		return BillingHold{}, err
	}
	if !hold.matches(usageID, walletID) || hold.Status != "ACTIVE" || hold.PriceVersionID == nil || *hold.PriceVersionID != priceID || *hold.MaxMicrocredits != maximum || hold.ActualMicrocredits != nil {
		return BillingHold{}, errors.New("Billing reservation facts do not match")
	}
	return hold, nil
}

func (h BillingHold) matches(usageID, walletID uuid.UUID) bool {
	return usageID != uuid.Nil && walletID != uuid.Nil && h.ID != uuid.Nil && h.UsageRequestID == usageID && h.WalletID == walletID && h.MaxMicrocredits != nil && *h.MaxMicrocredits >= 0 && !h.ExpiresAt.IsZero()
}

func (c *HTTPBillingClient) ReserveMemberQuotaPriced(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	var hold BillingHold
	body := holdReservationBody(usageID, walletID, maximum, priceID, pricing)
	body["organization_id"], body["member_user_id"], body["membership_version"] = organizationID, memberUserID, membershipVersion
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", body, &hold)
	if err != nil {
		return BillingHold{}, err
	}
	if !hold.matches(usageID, walletID) || hold.Status != "ACTIVE" || hold.PriceVersionID == nil || *hold.PriceVersionID != priceID || *hold.MaxMicrocredits != maximum || hold.ActualMicrocredits != nil {
		return BillingHold{}, errors.New("Billing reservation facts do not match")
	}
	return hold, nil
}

func (c *HTTPBillingClient) Extend(ctx context.Context, usageID, holdID, walletID uuid.UUID, maximum, additional int64) error {
	if maximum < 0 || additional < 0 || maximum > math.MaxInt64-additional {
		return ErrForbidden
	}
	var hold BillingHold
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/extend", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "lease_seconds": 900, "additional_microcredits": additional}, &hold); err != nil {
		return err
	}
	if !hold.matches(usageID, walletID) || hold.ID != holdID || hold.Status != "ACTIVE" || hold.ActualMicrocredits != nil || *hold.MaxMicrocredits != maximum+additional {
		return errors.New("Billing extension facts do not match")
	}
	return nil
}

func (c *HTTPBillingClient) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage, publishedAt time.Time) error {
	var result struct {
		ID     uuid.UUID `json:"formula_version_id"`
		Status string    `json:"status"`
	}
	if err := c.call(ctx, http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formulaID.String(), map[string]any{"protocol_code": protocol, "required_unit_definition_ids": required, "formula_snapshot": snapshot, "published_at": publishedAt}, &result); err != nil {
		return err
	}
	if result.ID != formulaID || formulaID == uuid.Nil || result.Status != "REGISTERED" {
		return errors.New("Billing formula binding facts do not match")
	}
	return nil
}

func (c *HTTPBillingClient) Commit(ctx context.Context, usageID, holdID, walletID uuid.UUID, actual int64, usage Usage) error {
	var hold BillingHold
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/commit", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "actual_microcredits": actual, "usage_snapshot": usage}, &hold); err != nil {
		return err
	}
	if !hold.matches(usageID, walletID) || hold.ID != holdID || hold.Status != "COMMITTED" || hold.ActualMicrocredits == nil || *hold.ActualMicrocredits != actual || actual < 0 || actual > *hold.MaxMicrocredits {
		return errors.New("Billing settlement facts do not match")
	}
	return nil
}

func (c *HTTPBillingClient) Release(ctx context.Context, usageID, holdID, walletID uuid.UUID, reason string) error {
	var hold BillingHold
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/release", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "reason": reason}, &hold); err != nil {
		return err
	}
	if !hold.matches(usageID, walletID) || hold.ID != holdID || hold.Status != "RELEASED" || (hold.ActualMicrocredits != nil && *hold.ActualMicrocredits != 0) {
		return errors.New("Billing release facts do not match")
	}
	return nil
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
	payload, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(payload) > 2<<20 {
		return errors.New("Billing response exceeds the allowed size")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Billing HTTP %d", response.StatusCode)
	}
	if output == nil {
		return errors.New("Billing response destination is required")
	}
	return json.Unmarshal(payload, output)
}

func MaximumUsage(maxInputTokens, maxOutputTokens int64) Usage {
	return Usage{InputTokens: maxInputTokens, OutputTokens: maxOutputTokens}
}
