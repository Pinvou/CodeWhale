package main

import "testing"

func TestExpiredHoldClientConfigurationFailsClosed(t *testing.T) {
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	t.Setenv("TOKEN_HOLD_EXPIRY_URL", "")
	client, err := expiredHoldClientFromEnvironment("fixture-workload-token")
	if err != nil || client != nil {
		t.Fatal("earlier wave must preserve manual-review fallback")
	}
	for _, endpoint := range []string{"http://token.example", "https://user:secret@token.example", "https://token.example?redirect=1", "https://token.example?", "https://:443", "https://token.example/%2f", "https://token.example/private"} {
		t.Setenv("TOKEN_HOLD_EXPIRY_URL", endpoint)
		if _, err := expiredHoldClientFromEnvironment("fixture-workload-token"); err == nil {
			t.Fatal("unsafe internal endpoint accepted")
		}
	}
	t.Setenv("TOKEN_HOLD_EXPIRY_URL", "https://token.example")
	if _, err := expiredHoldClientFromEnvironment(""); err == nil {
		t.Fatal("missing workload identity accepted")
	}
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "relative-untrusted-ca.pem")
	if _, err := expiredHoldClientFromEnvironment("fixture-workload-token"); err == nil {
		t.Fatal("unverified trust file accepted")
	}
}
