package billing

import (
	"context"
	"github.com/google/uuid"
)

func sameHoldPricing(ctx context.Context, database queryable, holdID uuid.UUID, pricing *HoldPricingContext) (bool, error) {
	var same bool
	if pricing == nil {
		err := database.QueryRow(ctx, `SELECT model_id IS NULL AND protocol_code IS NULL AND formula_version_id IS NULL AND pricing_snapshot IS NULL AND request_snapshot IS NULL FROM usage_holds WHERE id=$1`, holdID).Scan(&same)
		return same, err
	}
	err := database.QueryRow(ctx, `SELECT COALESCE(model_id=$2 AND protocol_code=$3 AND formula_version_id=$4 AND pricing_snapshot=$5::jsonb AND request_snapshot=$6::jsonb,false) FROM usage_holds WHERE id=$1`, holdID, pricing.ModelID, pricing.ProtocolCode, pricing.FormulaVersionID, []byte(pricing.PricingSnapshot), []byte(pricing.RequestSnapshot)).Scan(&same)
	return same, err
}
