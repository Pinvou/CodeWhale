package token

import (
	"math"

	"github.com/google/uuid"
)

func tokenCost(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 || (rate != 0 && tokens > (math.MaxInt64-999_999)/rate) {
		return 0, ErrForbidden
	}
	return (tokens*rate + 999_999) / 1_000_000, nil
}

func holdReservationBody(usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) map[string]any {
	return map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900, "model_id": pricing.ModelID, "protocol_code": pricing.ProtocolCode, "formula_version_id": pricing.FormulaVersionID, "pricing_snapshot": pricing.PricingSnapshot, "request_snapshot": pricing.RequestSnapshot}
}
