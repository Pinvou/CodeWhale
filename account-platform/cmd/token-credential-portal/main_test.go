package main

import "testing"

func TestCredentialEndpointConfiguration(t *testing.T) {
	for _, raw := range []string{"https://identity.example/realms/pinvou", "http://127.0.0.1:8080/oidc/callback"} {
		t.Run("valid_"+raw, func(t *testing.T) {
			if _, err := credentialURL(raw); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, raw := range []string{"", "https://", "https://user:secret@identity.example", "https://identity.example?", "https://identity.example?token=value", "https://identity.example#fragment", "ftp://identity.example", "identity.example"} {
		t.Run("invalid_"+raw, func(t *testing.T) {
			if _, err := credentialURL(raw); err == nil {
				t.Fatal("invalid URL accepted")
			}
		})
	}
}
