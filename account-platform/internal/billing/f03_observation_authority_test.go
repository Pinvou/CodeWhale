package billing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestF03ObservationPublicationAuthorityFormats(t *testing.T) {
	key := strings.Repeat("a", 64)
	for name, raw := range map[string]string{
		"historical_sql_null": "", "historical_json_null": "null", "historical_array": "[[1,2,3]]", "historical_empty_array": "[]",
	} {
		t.Run(name, func(t *testing.T) {
			versions, err := reconciliationObservedCaseVersions([]byte(raw))
			if err != nil || len(versions) != 0 {
				t.Fatalf("historical evidence must not gain reopen authority: %v %v", versions, err)
			}
		})
	}
	valid := `{"order_bindings":[],"case_versions":{"` + key + `":3}}`
	t.Run("new_frozen_version", func(t *testing.T) {
		versions, err := reconciliationObservedCaseVersions([]byte(valid))
		if err != nil || versions[key] != 3 || len(versions) != 1 {
			t.Fatal(versions, err)
		}
	})
	t.Run("original_seventeen_fields_unchanged", func(t *testing.T) {
		bindings := json.RawMessage(`[[null,"merchant",null,"provider","version","channel",9007199254740993,"CNY",9007199254740995,"PENDING","PENDING",null,null,null,9007199254740997,0,null]]`)
		raw, err := json.Marshal(reconciliationObservationSource{OrderBindings: bindings, CaseVersions: map[string]int64{key: 3}})
		if err != nil {
			t.Fatal(err)
		}
		var stored reconciliationObservationSource
		if err := json.Unmarshal(raw, &stored); err != nil || string(stored.OrderBindings) != string(bindings) {
			t.Fatal("financial integer lexemes or original tuple changed", string(raw), err)
		}
		if versions, err := reconciliationObservedCaseVersions(raw); err != nil || versions[key] != 3 {
			t.Fatal(versions, err)
		}
	})
	for name, raw := range map[string]string{
		"malformed_array": "[", "unrecognized_object": "{}", "missing_versions": `{"order_bindings":[]}`,
		"null_bindings": `{"order_bindings":null,"case_versions":{}}`, "object_bindings": `{"order_bindings":{},"case_versions":{}}`,
		"incomplete_tuple": `{"order_bindings":[[1]],"case_versions":{}}`, "zero_version": `{"order_bindings":[],"case_versions":{"` + key + `":0}}`,
		"negative_version": `{"order_bindings":[],"case_versions":{"` + key + `":-1}}`, "fractional_version": `{"order_bindings":[],"case_versions":{"` + key + `":1.5}}`,
		"invalid_digest": `{"order_bindings":[],"case_versions":{"x":3}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reconciliationObservedCaseVersions([]byte(raw)); !errors.Is(err, ErrConflict) {
				t.Fatal("invalid authority must fail closed", err)
			}
		})
	}
}
