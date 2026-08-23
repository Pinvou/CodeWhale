package token

import (
	"testing"

	"github.com/google/uuid"
)

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

func TestRequestIdempotencyKeyIsStableForDesktopRetries(t *testing.T) {
	resolved := AuthorizationContext{UserID: uuid.MustParse("0198f8e3-2d27-7a2b-93aa-4f1a8f796e8c"), SessionID: "oidc-session"}
	body := []byte(`{"model":"example-model","messages":[]}`)
	first := requestIdempotencyKey("", resolved, body)
	second := requestIdempotencyKey("", resolved, body)
	if first != second || len(first) != len("desktop-")+64 {
		t.Fatalf("unexpected derived key: %q %q", first, second)
	}
	if first == requestIdempotencyKey("", AuthorizationContext{UserID: resolved.UserID, SessionID: "other-session"}, body) {
		t.Fatal("OIDC sessions must not share an idempotency key")
	}
	if got := requestIdempotencyKey("explicit-request-key", resolved, body); got != "explicit-request-key" {
		t.Fatalf("explicit key changed: %q", got)
	}
}
