package token

import (
	"context"
	"encoding/json"

	"math"

	"net/http"

	"github.com/google/uuid"
)

type BillingHold struct {
	ID             uuid.UUID `json:"hold_id"`
	UsageRequestID uuid.UUID `json:"usage_request_id"`
	Status         string    `json:"status"`
}

type HoldPricingContext struct {
	ModelID          string
	ProtocolCode     string
	FormulaVersionID uuid.UUID
	PricingSnapshot  json.RawMessage
	RequestSnapshot  json.RawMessage
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

func (c *HTTPBillingClient) Release(ctx context.Context, usageID, holdID uuid.UUID, reason string) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/release", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "reason": reason}, nil)
}

type SnapshotBillingClient interface {
	ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, HoldPricingContext) (BillingHold, error)
}

type cancellationBillingClient interface {
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error)
	Release(context.Context, uuid.UUID, uuid.UUID, string) error
}
