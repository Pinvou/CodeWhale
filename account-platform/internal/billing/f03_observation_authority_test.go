package billing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestF03ObservationPublicationAuthorityFormats(t *testing.T) {
	key := strings.Repeat("a", 64)
	for name, raw := range map[string]string{
		"historical_sql_null": "", "historical_json_null": "null", "historical_array": "[[1,2,3]]", "historical_empty_array": "[]",
	} {
		t.Run(name, func(t *testing.T) {
			versions, _, err := reconciliationObservedCaseState([]byte(raw))
			if err != nil || len(versions) != 0 {
				t.Fatalf("historical evidence must not gain reopen authority: %v %v", versions, err)
			}
		})
	}
	valid := `{"order_bindings":[],"case_versions":{"` + key + `":3}}`
	t.Run("historical_frozen_version_without_case_binding", func(t *testing.T) {
		versions, ids, err := reconciliationObservedCaseState([]byte(valid))
		if err != nil || versions[key] != 3 || len(versions) != 1 || ids != nil {
			t.Fatal(versions, err)
		}
	})
	t.Run("original_seventeen_fields_unchanged", func(t *testing.T) {
		caseID := uuid.New()
		bindings := json.RawMessage(`[[null,"merchant",null,"provider","version","channel",9007199254740993,"CNY",9007199254740995,"PENDING","PENDING",null,null,null,9007199254740997,0,null]]`)
		raw, err := json.Marshal(reconciliationObservationSource{OrderBindings: bindings, CaseVersions: map[string]int64{key: 3}, CaseIDs: map[string]uuid.UUID{key: caseID}})
		if err != nil {
			t.Fatal(err)
		}
		var stored reconciliationObservationSource
		if err := json.Unmarshal(raw, &stored); err != nil || string(stored.OrderBindings) != string(bindings) {
			t.Fatal("financial integer lexemes or original tuple changed", string(raw), err)
		}
		if versions, ids, err := reconciliationObservedCaseState(raw); err != nil || versions[key] != 3 || ids[key] != caseID {
			t.Fatal(versions, err)
		}
	})
	for name, raw := range map[string]string{
		"malformed_array": "[", "unrecognized_object": "{}", "missing_versions": `{"order_bindings":[]}`,
		"null_bindings": `{"order_bindings":null,"case_versions":{}}`, "object_bindings": `{"order_bindings":{},"case_versions":{}}`,
		"incomplete_tuple": `{"order_bindings":[[1]],"case_versions":{}}`, "zero_version": `{"order_bindings":[],"case_versions":{"` + key + `":0}}`,
		"negative_version": `{"order_bindings":[],"case_versions":{"` + key + `":-1}}`, "fractional_version": `{"order_bindings":[],"case_versions":{"` + key + `":1.5}}`,
		"invalid_digest":             `{"order_bindings":[],"case_versions":{"x":3}}`,
		"invalid_case_uuid":          `{"order_bindings":[],"case_versions":{"` + key + `":3},"case_ids":{"` + key + `":"invalid"}}`,
		"zero_case_uuid":             `{"order_bindings":[],"case_versions":{"` + key + `":3},"case_ids":{"` + key + `":"00000000-0000-0000-0000-000000000000"}}`,
		"case_without_version":       `{"order_bindings":[],"case_versions":{},"case_ids":{"` + key + `":"11111111-1111-4111-8111-111111111111"}}`,
		"incomplete_case_bindings":   `{"order_bindings":[],"case_versions":{"` + key + `":3},"case_ids":{}}`,
		"different_case_binding_key": `{"order_bindings":[],"case_versions":{"` + key + `":3},"case_ids":{"` + strings.Repeat("b", 64) + `":"11111111-1111-4111-8111-111111111111"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := reconciliationObservedCaseState([]byte(raw)); !errors.Is(err, ErrConflict) {
				t.Fatal("invalid authority must fail closed", err)
			}
		})
	}
}
