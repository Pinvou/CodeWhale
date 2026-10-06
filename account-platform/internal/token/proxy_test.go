package token

import (
	"strings"
	"testing"
)

func TestParseUsageJSON(t *testing.T) {
	usage, ok := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":3}}}`))
	if !ok || usage.InputTokens != 8 || usage.OutputTokens != 7 || usage.CachedInputTokens != 3 {
		t.Fatalf("unexpected usage: %#v, %v", usage, ok)
	}
}

func TestIdempotencyKeyUsesUTF8CharacterContract(t *testing.T) {
	for _, test := range []struct {
		key  string
		want bool
	}{{strings.Repeat("界", 15), false}, {strings.Repeat("界", 16), true}, {strings.Repeat("界", 128), true}, {strings.Repeat("a", 129), false}, {strings.Repeat("界", 129), false}, {strings.Repeat("a", 15) + string([]byte{0xff}), false}} {
		if got := validIdempotencyKey(test.key); got != test.want {
			t.Fatalf("idempotency contract bytes=%d got=%v want=%v", len(test.key), got, test.want)
		}
	}
}

func TestParseUsageJSONRejectsMissingAndInvalidUsage(t *testing.T) {
	for _, payload := range []string{`{"choices":[]}`, `{"usage":{}}`, `{"usage":{"prompt_tokens":1}}`, `{"usage":{"prompt_tokens":null,"completion_tokens":0}}`, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`} {
		if _, ok := parseUsageJSON([]byte(payload)); ok {
			t.Fatalf("accepted invalid usage: %s", payload)
		}
	}
}
