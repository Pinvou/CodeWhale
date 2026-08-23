package token

import (
	"bytes"
	"encoding/json"
	"errors"
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
		EvaluatorVersion string            `json:"evaluator_version"`
		Expression       map[string]string `json:"expression"`
	}
	if json.Unmarshal(price.FormulaSnapshot, &formula) != nil || formula.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" {
		return ErrMeteringConfiguration
	}
	required := map[string]bool{"INPUT_TOKEN": false, "CACHED_INPUT_TOKEN": false, "OUTPUT_TOKEN": false}
	expectedParameters := map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "CACHED_INPUT_TOKEN": "cached_input_tokens", "OUTPUT_TOKEN": "max_output_tokens"}
	for _, unit := range price.Units {
		if _, ok := required[unit.UnitCode]; !ok || required[unit.UnitCode] || formula.Expression[unit.UnitCode] != expectedParameters[unit.UnitCode] {
			return ErrMeteringConfiguration
		}
		required[unit.UnitCode] = true
	}
	for _, found := range required {
		if !found {
			return ErrMeteringConfiguration
		}
	}
	return nil
}
