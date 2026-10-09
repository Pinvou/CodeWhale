package token

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRetainedCommandNumbersPreserveExactFacts(t *testing.T) {
	left := json.RawMessage(`{"version":9007199254740992}`)
	right := json.RawMessage(`{"version":9007199254740993}`)
	t.Run("canonical_integer", func(t *testing.T) {
		got, err := canonicalJSON(right)
		if err != nil || string(got) != string(right) {
			t.Fatalf("integer fact changed: %s, %v", got, err)
		}
	})
	t.Run("adjacent_versions", func(t *testing.T) {
		if tokenCommandExpectedVersionsMatch(left, right) || jsonObjectSubset(left, right) || jsonSemanticEqual(left, right) {
			t.Fatal("adjacent int64 versions matched")
		}
	})
	t.Run("decimal_exponent_equivalence", func(t *testing.T) {
		for _, pair := range [][2]string{{`{"n":1}`, `{"n":1.0}`}, {`{"n":1e0}`, `{"n":1}`}, {`{"n":-0}`, `{"n":0.00}`}, {`{"n":123.4500}`, `{"n":1.2345e2}`}, {`{"n":1e1000000}`, `{"n":10e999999}`}} {
			if !jsonSemanticEqual(json.RawMessage(pair[0]), json.RawMessage(pair[1])) {
				t.Fatalf("equivalent decimal facts differ: %v", pair)
			}
		}
	})
	t.Run("boundaries_nested_and_types", func(t *testing.T) {
		for _, raw := range []string{`{"a":[9223372036854775807,-9223372036854775808]}`, `{"version":"9007199254740993","unknown":true,"empty":null}`} {
			got, err := canonicalJSON(json.RawMessage(raw))
			if err != nil || !jsonSemanticEqual(got, json.RawMessage(raw)) {
				t.Fatalf("valid fact changed: %s", raw)
			}
		}
		if jsonSemanticEqual(json.RawMessage(`{"n":1}`), json.RawMessage(`{"n":"1"}`)) || jsonSemanticEqual(json.RawMessage(`{"n":1}`), json.RawMessage(`{"n":true}`)) {
			t.Fatal("JSON types were coerced")
		}
	})
	t.Run("complete_single_json", func(t *testing.T) {
		for _, raw := range []string{`{"version":1} {}`, `{"version":1} trailing`, `{"version":`} {
			if _, err := canonicalJSON(json.RawMessage(raw)); err == nil {
				t.Fatalf("incomplete/multiple JSON accepted: %s", raw)
			}
			if tokenCommandExpectedVersionsMatch(json.RawMessage(raw), left) || jsonObjectSubset(json.RawMessage(raw), left) {
				t.Fatal("invalid comparison accepted")
			}
		}
		if tokenCommandExpectedVersionsMatch(json.RawMessage(`null`), json.RawMessage(`null`)) || jsonObjectSubset(json.RawMessage(`null`), json.RawMessage(`{}`)) {
			t.Fatal("null version object accepted")
		}
	})
	t.Run("typed_int64_constraints", func(t *testing.T) {
		for _, raw := range []string{`{"version":9223372036854775808}`, `{"version":1.5}`, `{"version":"1"}`} {
			var value struct {
				Version int64 `json:"version"`
			}
			if decodeExactCommandJSON(json.RawMessage(raw), &value) == nil {
				t.Fatalf("typed int64 constraint lost: %s", raw)
			}
		}
	})
}

func TestRetainedNormalizeTokenCommandBindsRawIntegerFacts(t *testing.T) {
	envelope := TokenCommandEnvelope{CommandType: "TOKEN_ACCOUNT_STATUS_CHANGE", Reason: "disable account after impact review", TargetResourceType: "TOKEN_ACCOUNT", TargetResourceID: "01a00000-0000-7000-8000-000000000001", PrecheckFingerprint: strings.Repeat("a", 64), Command: json.RawMessage(`{"target_status":"DISABLED"}`)}
	makeFacts := func(version string) json.RawMessage {
		raw := json.RawMessage(`{"token_account_id":"` + envelope.TargetResourceID + `","expected_account_version":` + version + `,"target_status":"DISABLED","precheck_fingerprint":"` + envelope.PrecheckFingerprint + `"}`)
		var facts map[string]any
		if err := decodeExactCommandJSON(raw, &facts); err != nil {
			t.Fatal(err)
		}
		facts["request_fingerprint"] = governedRequestFingerprint(envelope, facts)
		encoded, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	envelope.ExpectedVersions = makeFacts("9007199254740993")
	_, first, _, _, err := normalizeTokenCommand(envelope, "stable-command-key-0001", "")
	if err != nil {
		t.Fatalf("exact int64 raw command rejected: %v", err)
	}
	// Change only the version while keeping the original proof: actual normalize
	// must reject before any command mutation or new receipt can be produced.
	envelope.ExpectedVersions = json.RawMessage(strings.Replace(string(envelope.ExpectedVersions), "9007199254740993", "9007199254740992", 1))
	if _, _, _, _, err := normalizeTokenCommand(envelope, "stable-command-key-0001", ""); err == nil {
		t.Fatal("adjacent fact reused the original fingerprint")
	}
	envelope.ExpectedVersions = makeFacts("9007199254740992")
	_, second, _, _, err := normalizeTokenCommand(envelope, "stable-command-key-0001", "")
	if err != nil || first == second {
		t.Fatalf("distinct exact raw facts did not bind separate receipts: %v", err)
	}
}
