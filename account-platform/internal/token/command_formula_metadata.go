package token

import (
	"strings"

	"github.com/google/uuid"
)

// Only a complete safe formula pair establishes the unit-key context. The
// caller still scans every other field and every non-semantic unit name.
// This is not a global exemption for fields whose names contain "token".
func validGovernedFormulaUnitMaps(object map[string]any) bool {
	required, requiredOK := object["required_unit_versions"].(map[string]any)
	expression, expressionOK := object["expression"].(map[string]any)
	if !requiredOK || !expressionOK || len(required) == 0 || len(required) > 32 || len(required) != len(expression) {
		return false
	}
	typedRequired := make(map[string]uuid.UUID, len(required))
	typedExpression := make(map[string]string, len(expression))
	for unit, raw := range required {
		text, ok := raw.(string)
		if !ok {
			return false
		}
		id, err := uuid.Parse(text)
		if err != nil || id == uuid.Nil {
			return false
		}
		typedRequired[unit] = id
	}
	for unit, raw := range expression {
		text, ok := raw.(string)
		if !ok {
			return false
		}
		typedExpression[unit] = text
	}
	_, _, err := normalizeSafeFormula(typedRequired, typedExpression)
	return err == nil
}

func governedTokenMeteringUnit(unit string) bool {
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN":
		return true
	default:
		return false
	}
}
