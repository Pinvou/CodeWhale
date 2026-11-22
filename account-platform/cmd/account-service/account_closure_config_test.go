package main

import "testing"

// The existing factory is configuration-only: no HTTP call, PG connection or
// closure request is made by this test.
func TestConfiguredAccountClosureAuthorities(t *testing.T) {
	for _, tc := range []struct {
		name, billing, token, credential string
		valid                            bool
	}{
		{"valid", "http://billing.invalid:8080", "http://token.invalid:8080", "controlled-closure-credential", true},
		{"billing-missing", "", "http://token.invalid:8080", "controlled-closure-credential", false},
		{"token-missing", "http://billing.invalid:8080", "", "controlled-closure-credential", false},
		{"foreign-token-authority", "http://billing.invalid:8080", "http://user@token.invalid:8080", "controlled-closure-credential", false},
		{"missing-credential", "http://billing.invalid:8080", "http://token.invalid:8080", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BILLING_HTTP_URL", tc.billing)
			t.Setenv("TOKEN_HTTP_URL", tc.token)
			billing, token, err := configuredAccountClosureClients(tc.credential)
			if tc.valid {
				if err != nil || billing == nil || token == nil {
					t.Fatal("valid closure authorities not configured", err)
				}
			} else if err == nil || billing != nil || token != nil {
				t.Fatal("invalid authority did not fail closed")
			}
		})
	}
}
