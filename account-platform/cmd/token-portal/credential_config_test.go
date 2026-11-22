package main

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
	"testing"
)

func TestF04PortalCredentialConfigurationUsesIsolatedFixedOrigin(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example", "PORTAL_ADMIN_TOKEN_DETAILS_ENABLED": "true", "CREDENTIAL_PORTAL_PUBLIC_URL": "https://credentials-uat.pinvou.com"}
	get := func(name string) string { return env[name] }
	config, err := loadPortalConfig("production", get)
	if err != nil || !config.AdminTokenDetailsEnabled || config.CredentialOrigin == nil || config.CredentialOrigin.String() != env["CREDENTIAL_PORTAL_PUBLIC_URL"] {
		t.Fatal("isolated credential config was not wired")
	}
	store, err := portal.NewRedisStore("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = portal.NewService(config, store); err != nil {
		t.Fatal(err)
	}
	env["PORTAL_ADMIN_TOKEN_DETAILS_ENABLED"] = "not-a-boolean"
	if _, err = loadPortalConfig("production", get); err == nil {
		t.Fatal("invalid enable flag accepted")
	}
	env["PORTAL_ADMIN_TOKEN_DETAILS_ENABLED"] = "true"
	for _, raw := range []string{"", "http://credentials-uat.pinvou.com", "https://untrusted.example", "https://credentials-uat.pinvou.com/extra"} {
		env["CREDENTIAL_PORTAL_PUBLIC_URL"] = raw
		config, err = loadPortalConfig("production", get)
		if err == nil {
			_, err = portal.NewService(config, store)
		}
		if err == nil {
			t.Errorf("invalid isolated origin accepted: %s", raw)
		}
	}
}
