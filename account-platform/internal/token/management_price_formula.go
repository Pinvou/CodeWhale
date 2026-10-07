package token

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrMeteringConfiguration = errors.New("metering configuration is invalid")

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
