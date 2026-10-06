package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"

	"github.com/google/uuid"
)

var ErrMeteringConfiguration = errors.New("metering configuration is invalid")

// EstimateChatUsage applies the published CHAT v1 estimator. It intentionally
// uses a versioned, deterministic byte rule instead of a provider-dependent
// tokenizer so the same request always receives the same preauthorization.
func EstimateChatUsage(payload []byte, maxOutputTokens int64) (Usage, error) {
	if maxOutputTokens < 0 {
		return Usage{}, ErrMeteringConfiguration
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var request struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := decoder.Decode(&request); err != nil {
		return Usage{}, ErrMeteringConfiguration
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return Usage{}, ErrMeteringConfiguration
	}
	var bytesTotal int64
	for _, message := range request.Messages {
		count, err := meteredContentBytes(message.Content)
		if err != nil || bytesTotal > math.MaxInt64-count {
			return Usage{}, ErrMeteringConfiguration
		}
		bytesTotal += count
	}
	inputTokens := bytesTotal / 4
	if bytesTotal%4 != 0 {
		inputTokens++
	}
	return Usage{InputTokens: inputTokens, OutputTokens: maxOutputTokens}, nil
}

func meteredContentBytes(content any) (int64, error) {
	switch value := content.(type) {
	case nil:
		return 0, nil
	case string:
		return int64(len([]byte(value))), nil
	case []any:
		var total int64
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return 0, ErrMeteringConfiguration
			}
			text, _ := part["text"].(string)
			if total > math.MaxInt64-int64(len([]byte(text))) {
				return 0, ErrMeteringConfiguration
			}
			total += int64(len([]byte(text)))
		}
		return total, nil
	default:
		return 0, ErrMeteringConfiguration
	}
}

func ValidatePriceFormula(price PriceVersion) error {
	if price.ID == uuid.Nil || price.FormulaVersionID == uuid.Nil || price.ProtocolCode != "CHAT" || price.CreditUnit != "MICROCREDIT" || !json.Valid(price.FormulaSnapshot) {
		return ErrMeteringConfiguration
	}
	var formula struct {
		EvaluatorVersion     string               `json:"evaluator_version"`
		Expression           map[string]string    `json:"expression"`
		RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
	}
	if json.Unmarshal(price.FormulaSnapshot, &formula) != nil || formula.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || len(formula.Expression) != 3 || len(formula.RequiredUnitVersions) != 3 {
		return ErrMeteringConfiguration
	}
	required := map[string]bool{"INPUT_TOKEN": false, "CACHED_INPUT_TOKEN": false, "OUTPUT_TOKEN": false}
	seenDefinitions := make(map[uuid.UUID]bool, len(required))
	expectedParameters := map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "CACHED_INPUT_TOKEN": "cached_input_tokens", "OUTPUT_TOKEN": "max_output_tokens"}
	for _, unit := range price.Units {
		if _, ok := required[unit.UnitCode]; !ok || required[unit.UnitCode] || seenDefinitions[unit.UnitDefinitionID] || formula.Expression[unit.UnitCode] != expectedParameters[unit.UnitCode] || unit.UnitDefinitionID == uuid.Nil || formula.RequiredUnitVersions[unit.UnitCode] != unit.UnitDefinitionID || unit.UnitVersion < 1 || unit.MinimumPrecision < 1 || unit.PriceDenominator < 1 || unit.ResolvedPrice < 0 || unit.SourcePriceVersionID == uuid.Nil {
			return ErrMeteringConfiguration
		}
		if unit.RoundingRule != "CEILING" && unit.RoundingRule != "FLOOR" && unit.RoundingRule != "HALF_UP" {
			return ErrMeteringConfiguration
		}
		if unit.ConfigurationType != "FIXED_PRICE" && unit.ConfigurationType != "INHERIT_GLOBAL" {
			return ErrMeteringConfiguration
		}
		if unit.PriceSourceScope != "GLOBAL" && unit.PriceSourceScope != "MODEL" {
			return ErrMeteringConfiguration
		}
		required[unit.UnitCode] = true
		seenDefinitions[unit.UnitDefinitionID] = true
	}
	for _, found := range required {
		if !found {
			return ErrMeteringConfiguration
		}
	}
	return nil
}
