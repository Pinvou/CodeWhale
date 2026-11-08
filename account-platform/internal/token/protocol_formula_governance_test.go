package token

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProtocolFormulaGovernedEnvelopeBindsCanonicalPayload(t *testing.T) {
	envelope := TokenCommandEnvelope{
		CommandType:         "TOKEN_PROTOCOL_DRAFT_CREATE",
		Reason:              "create reviewed protocol definition draft",
		TargetResourceType:  "PROTOCOL",
		TargetResourceID:    "CHAT_V2",
		PrecheckFingerprint: strings.Repeat("a", 64),
		Command:             json.RawMessage(`{"protocol_code":"CHAT_V2","request_schema":{"type":"object"}}`),
	}
	facts := map[string]any{"protocol_code": "CHAT_V2", "expected_protocol_head_version": float64(0)}
	facts["request_fingerprint"] = governedRequestFingerprint(envelope, facts)
	envelope.ExpectedVersions, _ = json.Marshal(facts)
	_, requestDigest, _, idempotencyFingerprint, err := normalizeTokenCommand(envelope, "protocol-draft-key-0001", "")
	if err != nil {
		t.Fatalf("valid governed protocol command rejected: %v", err)
	}
	if requestDigest != "8e3b90f49f81a4bd27381c4bf3e820a3987f0bd364f11332a17d65380df4c341" || idempotencyFingerprint != "84e70c4944f11da4cdaaceb5bc42e221ae7d80d371ec919d45c8c2452f9f60b1" {
		t.Fatalf("canonical vector drifted: request=%s idempotency=%s", requestDigest, idempotencyFingerprint)
	}

	t.Run("equivalent JSON and normalized code", func(t *testing.T) {
		equivalent := envelope
		equivalent.Command = json.RawMessage(" { \n \"request_schema\" : { \"type\" : \"object\" }, \"protocol_code\" : \" chat_v2 \" } ")
		equivalent.ExpectedVersions = json.RawMessage(`{"request_fingerprint":"` + facts["request_fingerprint"].(string) + `","expected_protocol_head_version":0,"protocol_code":" chat_v2 "}`)
		_, equivalentRequest, _, equivalentFingerprint, equivalentErr := normalizeTokenCommand(equivalent, "protocol-draft-key-0001", "")
		if equivalentErr != nil || equivalentRequest != requestDigest || equivalentFingerprint != idempotencyFingerprint {
			t.Fatalf("equivalent payload changed identity: err=%v request=%s fingerprint=%s", equivalentErr, equivalentRequest, equivalentFingerprint)
		}
	})

	t.Run("changed command", func(t *testing.T) {
		changed := envelope
		changed.Command = json.RawMessage(`{"protocol_code":"CHAT_V2","request_schema":{"type":"string"}}`)
		if _, _, _, _, err := normalizeTokenCommand(changed, "protocol-draft-key-0001", ""); err == nil {
			t.Fatal("changed command reused frozen request fingerprint")
		}
	})
	t.Run("changed expected facts", func(t *testing.T) {
		changed := envelope
		changed.ExpectedVersions = json.RawMessage(`{"protocol_code":"CHAT_V2","expected_protocol_head_version":1,"request_fingerprint":"` + facts["request_fingerprint"].(string) + `"}`)
		if _, _, _, _, err := normalizeTokenCommand(changed, "protocol-draft-key-0001", ""); err == nil {
			t.Fatal("changed authoritative fact reused frozen request fingerprint")
		}
	})
	t.Run("extra command field", func(t *testing.T) {
		changed := envelope
		changed.Command = json.RawMessage(`{"protocol_code":"CHAT_V2","request_schema":{"type":"object"},"secret":"hidden"}`)
		if _, _, _, _, err := normalizeTokenCommand(changed, "protocol-draft-key-0001", ""); err == nil {
			t.Fatal("extra command field was accepted")
		}
	})
}

func TestProtocolFormulaTerminalFailureClassifiesOnlyStableBillingResponses(t *testing.T) {
	for _, testCase := range []struct {
		status int
		code   string
		final  bool
	}{
		{status: 409, code: "BILLING_BINDING_CONFLICT", final: true},
		{status: 422, code: "FORMULA_UNIT_VALIDATION_REJECTED", final: true},
		{status: 429, final: false},
		{status: 503, final: false},
	} {
		code, final := protocolFormulaTerminalFailure(&BillingHTTPError{Status: testCase.status, Code: "REDACTED"})
		if final != testCase.final || code != testCase.code {
			t.Fatalf("unexpected Billing %d classification: code=%q final=%t", testCase.status, code, final)
		}
	}
}
