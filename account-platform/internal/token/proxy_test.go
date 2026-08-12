package token

import "testing"

func TestParseUsageJSON(t *testing.T) {
	usage, ok := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if !ok || usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.CachedInputTokens != 3 {
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
