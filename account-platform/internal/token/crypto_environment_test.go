package token

import (
	"bytes"
	"testing"
)

func TestSecretProviderUsesCanonicalProductionClassification(t *testing.T) {
	for _, environment := range []string{"production", "production-new", "production-eu", " Production-East "} {
		t.Run(environment, func(t *testing.T) {
			for _, mode := range []string{"", "local"} {
				if _, err := NewSecretProvider(SecretProviderConfig{Environment: environment, Mode: mode, KeyID: "controlled", LocalKey: bytes.Repeat([]byte{1}, 32)}); err == nil {
					t.Fatal("production variant accepted a local key")
				}
			}
			if _, err := NewRemoteEnvelopeProvider(environment, "controlled", "http://kms.example.invalid", "controlled-fixture-token-32-characters"); err == nil {
				t.Fatal("production variant accepted plaintext KMS")
			}
			if _, err := NewRemoteEnvelopeProvider(environment, "controlled", "https://kms.example.invalid", "controlled-fixture-token-32-characters"); err != nil {
				t.Fatal("HTTPS KMS configuration was rejected", err)
			}
			policy, err := NewProviderURLPolicy(environment, []string{"newapi.example.invalid"})
			if err != nil {
				t.Fatal(err)
			}
			if policy.Validate("http://newapi.example.invalid") == nil || policy.Validate("https://newapi.example.invalid") != nil || policy.Validate("https://unlisted.example.invalid") == nil {
				t.Fatal("production provider URL bypassed HTTPS or host allowlist")
			}
		})
	}
	for _, environment := range []string{"development", "test", "staging", ""} {
		t.Run("nonproduction-"+environment, func(t *testing.T) {
			if _, err := NewSecretProvider(SecretProviderConfig{Environment: environment, KeyID: "controlled", LocalKey: bytes.Repeat([]byte{1}, 32)}); err != nil {
				t.Fatal("nonproduction local configuration changed", err)
			}
			if _, err := NewRemoteEnvelopeProvider(environment, "controlled", "http://kms.example.invalid", "controlled-fixture-token-32-characters"); err != nil {
				t.Fatal("nonproduction HTTP configuration changed", err)
			}
			policy, err := NewProviderURLPolicy(environment, []string{"newapi.example.invalid"})
			if err != nil || policy.Validate("http://newapi.example.invalid") != nil || policy.Validate("http://unlisted.example.invalid") == nil {
				t.Fatal("nonproduction provider URL or allowlist changed", err)
			}
		})
	}
}
