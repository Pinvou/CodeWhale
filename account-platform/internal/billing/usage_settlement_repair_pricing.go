package billing

import (
	"encoding/json"
	"github.com/google/uuid"
	"math"
)

type usageRepairUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

func usageRepairFrozenPricing(priceID *uuid.UUID, model, protocol string, formulaID *uuid.UUID, pricingJSON, usageJSON []byte) (int64, error) {
	var priceKeys map[string]json.RawMessage
	if json.Unmarshal(pricingJSON, &priceKeys) != nil {
		return 0, ErrApprovalSnapshotMismatch
	}
	for _, key := range []string{"price_version_id", "model", "protocol_code", "formula_version_id", "credit_unit", "input_per_million", "output_per_million", "cached_input_per_million"} {
		if len(priceKeys[key]) == 0 || string(priceKeys[key]) == "null" {
			return 0, ErrApprovalSnapshotMismatch
		}
	}
	if len(priceKeys["units"]) > 0 && string(priceKeys["units"]) != "null" {
		var units []map[string]json.RawMessage
		if json.Unmarshal(priceKeys["units"], &units) != nil {
			return 0, ErrApprovalSnapshotMismatch
		}
		for _, u := range units {
			for _, key := range []string{"unit_code", "minimum_precision", "price_denominator", "resolved_price", "rounding_rule"} {
				if len(u[key]) == 0 || string(u[key]) == "null" {
					return 0, ErrApprovalSnapshotMismatch
				}
			}
		}
	}
	var price PriceVersion
	var usage usageRepairUsage
	if priceID == nil || *priceID == uuid.Nil || json.Unmarshal(pricingJSON, &price) != nil || price.ID != *priceID || price.Model != model || model == "" || price.ProtocolCode != protocol || protocol == "" || formulaID == nil || *formulaID == uuid.Nil || price.FormulaVersionID != *formulaID || price.CreditUnit != "MICROCREDIT" {
		return 0, ErrApprovalSnapshotMismatch
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(usageJSON, &keys) != nil || keys["input_tokens"] == nil || keys["output_tokens"] == nil || keys["cached_input_tokens"] == nil || string(keys["input_tokens"]) == "null" || string(keys["output_tokens"]) == "null" || string(keys["cached_input_tokens"]) == "null" || json.Unmarshal(usageJSON, &usage) != nil {
		return 0, ErrApprovalSnapshotMismatch
	}
	return usageRepairCost(price, usage)
}

func usageRepairCost(price PriceVersion, usage usageRepairUsage) (int64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return 0, ErrApprovalSnapshotMismatch
	}
	if len(price.Units) == 0 {
		input, err := usageRepairTokenCost(usage.InputTokens, price.InputPerMillion)
		if err != nil {
			return 0, err
		}
		cached, err := usageRepairTokenCost(usage.CachedInputTokens, price.CachedInputPerMillion)
		if err != nil || input > math.MaxInt64-cached {
			return 0, ErrApprovalSnapshotMismatch
		}
		output, err := usageRepairTokenCost(usage.OutputTokens, price.OutputPerMillion)
		if err != nil || input+cached > math.MaxInt64-output {
			return 0, ErrApprovalSnapshotMismatch
		}
		return input + cached + output, nil
	}
	quantities := map[string]int64{"INPUT_TOKEN": usage.InputTokens, "CACHED_INPUT_TOKEN": usage.CachedInputTokens, "OUTPUT_TOKEN": usage.OutputTokens}
	var total int64
	for _, unit := range price.Units {
		quantity, ok := quantities[unit.UnitCode]
		if !ok || unit.MinimumPrecision <= 0 || unit.PriceDenominator <= 0 || unit.ResolvedPrice < 0 {
			return 0, ErrApprovalSnapshotMismatch
		}
		component, err := usageRepairUnitCost(quantity, unit)
		if err != nil || total > math.MaxInt64-component {
			return 0, ErrApprovalSnapshotMismatch
		}
		total += component
		delete(quantities, unit.UnitCode)
	}
	if len(quantities) != 0 {
		return 0, ErrApprovalSnapshotMismatch
	}
	return total, nil
}

func usageRepairUnitCost(quantity int64, unit ResolvedUnitPrice) (int64, error) {
	if quantity < 0 || unit.ResolvedPrice < 0 || unit.PriceDenominator <= 0 || unit.MinimumPrecision <= 0 || quantity > math.MaxInt64/unit.MinimumPrecision {
		return 0, ErrApprovalSnapshotMismatch
	}
	precisionUnits := quantity / unit.MinimumPrecision
	if quantity%unit.MinimumPrecision != 0 {
		precisionUnits++
	}
	if unit.ResolvedPrice != 0 && precisionUnits > math.MaxInt64/unit.ResolvedPrice {
		return 0, ErrApprovalSnapshotMismatch
	}
	numerator := precisionUnits * unit.ResolvedPrice
	switch unit.RoundingRule {
	case "CEILING":
		if numerator > math.MaxInt64-(unit.PriceDenominator-1) {
			return 0, ErrApprovalSnapshotMismatch
		}
		return (numerator + unit.PriceDenominator - 1) / unit.PriceDenominator, nil
	case "FLOOR":
		return numerator / unit.PriceDenominator, nil
	case "HALF_UP":
		if numerator > math.MaxInt64-unit.PriceDenominator/2 {
			return 0, ErrApprovalSnapshotMismatch
		}
		return (numerator + unit.PriceDenominator/2) / unit.PriceDenominator, nil
	default:
		return 0, ErrApprovalSnapshotMismatch
	}
}

func usageRepairTokenCost(tokens, rate int64) (int64, error) {
	if tokens < 0 || rate < 0 || (rate != 0 && tokens > (math.MaxInt64-999_999)/rate) {
		return 0, ErrApprovalSnapshotMismatch
	}
	return (tokens*rate + 999_999) / 1_000_000, nil
}
