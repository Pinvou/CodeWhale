package token

import (
	"encoding/json"

	"testing"

	"github.com/google/uuid"
)

func TestGovernedFormulaUnitMetadataIsContextBound(t *testing.T) {
	id := uuid.NewString()
	for _, unit := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN", " input_token ", "REQUEST"} {
		pair := map[string]any{"required_unit_versions": map[string]string{unit: id}, "expression": map[string]string{unit: "estimated_input_tokens"}}
		for _, value := range []any{pair, map[string]any{"before_state": []any{pair}}} {
			raw, _ := json.Marshal(value)
			if containsSensitiveCommandField(raw) {
				t.Fatalf("safe formula unit %q rejected", unit)
			}
		}
	}
	bad := []any{
		map[string]any{"INPUT_TOKEN": id},
		map[string]any{"expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}},
		map[string]any{"Required_Unit_Versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": uuid.Nil.String()}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": "a-private-value"}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "eval(private_input)"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"OUTPUT_TOKEN": "estimated_input_tokens"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id, "input_token": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens", "input_token": "request_count"}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]any{"INPUT_TOKEN": map[string]string{"secret": "hidden"}}},
		map[string]any{"required_unit_versions": map[string]string{"INPUT_TOKEN": id}, "expression": map[string]string{"INPUT_TOKEN": "estimated_input_tokens"}, "nested": []any{map[string]string{"accessToken": "private"}}},
	}
	for _, key := range []string{"ACCESS_TOKEN", "api_key", "private_key", "SESSION_TOKEN", "INPUT_TOKEN_SECRET", "cookie"} {
		bad = append(bad, map[string]any{"required_unit_versions": map[string]string{key: id}, "expression": map[string]string{key: "request_count"}})
	}
	for index, value := range bad {
		raw, _ := json.Marshal(value)
		if !containsSensitiveCommandField(raw) {
			t.Fatalf("unsafe formula context %d accepted", index)
		}
	}
	for _, key := range []string{"INPUT_TOKEN", "CACHED_INPUT_TOKEN", "OUTPUT_TOKEN", "access_token", "api_key"} {
		if !sensitiveTokenGovernanceFieldName(normalizeTokenSensitiveFieldName(key)) {
			t.Fatalf("global sensitive-field rule was relaxed for %s", key)
		}
	}
}
