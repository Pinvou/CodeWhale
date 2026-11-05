package approvalencoding

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestIndependentAuthorizationDecoderBoundary(t *testing.T) {
	body, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture goldenFile
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	var vector *goldenAuthorizationVector
	for index := range fixture.AuthorizationVectors {
		if fixture.AuthorizationVectors[index].SerializationVersion == ApprovalSerializationVersion {
			vector = &fixture.AuthorizationVectors[index]
			break
		}
	}
	if vector == nil {
		t.Fatal("missing new authorization fixture")
	}
	version, err := strconv.ParseInt(vector.ApprovedRequestVersion, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := strconv.ParseInt(vector.ApprovalEpoch, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	expected := ExpectedAuthorizationBinding{ApprovedRequestVersion: version, ApprovalEpoch: epoch}
	if _, err := DecodeAuthorizationEvent([]byte(vector.EventJSON), expected); err != nil {
		t.Fatal(err)
	}
	for name, binding := range map[string]ExpectedAuthorizationBinding{
		"wrong request version": {ApprovedRequestVersion: version - 1, ApprovalEpoch: epoch},
		"wrong approval epoch":  {ApprovedRequestVersion: version, ApprovalEpoch: epoch - 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAuthorizationEvent([]byte(vector.EventJSON), binding); err == nil {
				t.Fatal("accepted incorrect external binding")
			}
		})
	}
	cases := []struct{ name, old, replacement string }{
		{"unknown discriminator", ApprovalSerializationVersion, "UNKNOWN_WIRE"},
		{"null discriminator", `"serialization_version":"` + ApprovalSerializationVersion + `"`, `"serialization_version":null`},
		{"missing discriminator", `"serialization_version":"` + ApprovalSerializationVersion + `",`, ""},
		{"duplicate envelope", `{`, `{"event_id":"00000000-0000-0000-0000-000000000000",`},
		{"unknown envelope", `{`, `{"unknown":true,`},
		{"duplicate payload", `"payload":{`, `"payload":{"policy_version":1,`},
		{"secret payload", `"payload":{`, `"payload":{"secret":"redacted-test-only",`},
		{"unknown authorization schema", `"authorization_schema_version":"1"`, `"authorization_schema_version":"2"`},
		{"unknown proof schema", `"proof_schema_version":"1"`, `"proof_schema_version":"2"`},
		{"numeric aggregate version", `"aggregate_version":"` + vector.ApprovedRequestVersion + `"`, `"aggregate_version":` + vector.ApprovedRequestVersion},
		{"numeric approval epoch", `"approval_control_epoch":"` + vector.ApprovalEpoch + `"`, `"approval_control_epoch":` + vector.ApprovalEpoch},
		{"nested unknown", `"normalized_command":{`, `"normalized_command":{"unknown":false,`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(vector.EventJSON, test.old, test.replacement, 1)
			if raw == vector.EventJSON {
				t.Fatal("fixture mutation did not match")
			}
			if _, err := DecodeAuthorizationEvent([]byte(raw), expected); err == nil {
				t.Fatal("accepted malformed or non-closed authorization")
			}
		})
	}
	// Semantic decoding does not replace the raw-byte digest check owned by the
	// authorization consumer. Transport whitespace is allowed by this decoder.
	if _, err := DecodeAuthorizationEvent([]byte(" \n"+vector.EventJSON+"\n"), expected); err != nil {
		t.Fatalf("equivalent transport whitespace rejected: %v", err)
	}
}
