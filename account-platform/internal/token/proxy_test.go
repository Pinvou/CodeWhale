package token

import (
	"net/http"
	"testing"
)

func TestGatewayAdaptersOwnProtocolAndReferenceMapping(t *testing.T) {
	response := &http.Response{Header: http.Header{"X-Request-Id": []string{"generic-request"}, "X-Oneapi-Request-Id": []string{"newapi-request"}}}
	generic := NewHTTPUpstreamClient()
	newAPI := NewNewAPIGatewayAdapter()
	if generic.UpstreamRequestID(response) != "generic-request" || newAPI.UpstreamRequestID(response) != "newapi-request" {
		t.Fatal("provider response headers escaped their gateway adapters")
	}
	forwarded := http.Header{}
	newAPI.CopyResponseHeaders(forwarded, response.Header)
	if forwarded.Get("X-Oneapi-Request-Id") != "" || forwarded.Get("X-Request-Id") != "generic-request" {
		t.Fatal("provider-specific evidence headers must not escape the selected adapter")
	}
	protocols := newAPI.Protocols()
	if len(protocols) != 1 || protocols[0].ProtocolCode != "CHAT" || protocols[0].ProtocolVersion != 1 || !protocols[0].Streaming {
		t.Fatalf("unexpected protocol capabilities: %#v", protocols)
	}
}

func TestNormalizeLogicalTurnID(t *testing.T) {
	valid := "pinvou-turn-0123456789abcdef"
	if got, err := normalizeLogicalTurnID(valid); err != nil || got != valid {
		t.Fatalf("logical turn ID = %q, %v", got, err)
	}
	if got, err := normalizeLogicalTurnID(""); err != nil || got != "" {
		t.Fatalf("empty logical turn ID = %q, %v", got, err)
	}
	for _, invalid := range []string{"short", "contains spaces and punctuation!"} {
		if _, err := normalizeLogicalTurnID(invalid); err == nil {
			t.Fatalf("accepted invalid logical turn ID %q", invalid)
		}
	}
}

func TestParseUsageJSON(t *testing.T) {
	usage, ok := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if !ok || usage.InputTokens != 8 || usage.OutputTokens != 7 || usage.CachedInputTokens != 3 {
		t.Fatalf("unexpected usage: %#v, %v", usage, ok)
	}
}

func TestParseUsageJSONRejectsMissingAndInvalidUsage(t *testing.T) {
	for _, payload := range []string{`{"choices":[]}`, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`} {
		if _, ok := parseUsageJSON([]byte(payload)); ok {
			t.Fatalf("accepted invalid usage: %s", payload)
		}
	}
}

func TestInferenceFingerprintIsCanonicalAndTransportIndependent(t *testing.T) {
	first, err := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"temperature":0.5}`), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeInferenceBody([]byte(`{"temperature":0.5,"messages":[],"model":"example-model"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if inferenceFingerprint("CHAT", 1, first) != inferenceFingerprint("CHAT", 1, second) {
		t.Fatal("equivalent JSON requests produced different fingerprints")
	}
	changed, _ := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"temperature":0.7}`), false)
	if inferenceFingerprint("CHAT", 1, first) == inferenceFingerprint("CHAT", 1, changed) {
		t.Fatal("semantic request changes must change the fingerprint")
	}
}

func TestStreamingCanonicalizationForcesUsageEvidence(t *testing.T) {
	body, err := normalizeInferenceBody([]byte(`{"model":"example-model","messages":[],"stream":true}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"messages":[],"model":"example-model","stream":true,"stream_options":{"include_usage":true}}` {
		t.Fatalf("unexpected canonical stream request: %s", body)
	}
}
