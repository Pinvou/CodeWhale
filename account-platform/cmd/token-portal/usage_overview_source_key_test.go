package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRetainedF04OverviewProxyKeyConfig(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example"}
	for _, tc := range []struct {
		name, key, flag string
		valid           bool
	}{
		{"disabled-absent", "", "false", true}, {"enabled-absent", "", "true", false}, {"malformed", "not-base64", "false", false}, {"short", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 31))), "true", false}, {"enabled-valid", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))), "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env["TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64"] = tc.key
			env["PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED"] = tc.flag
			cfg, err := loadPortalConfig("production", func(n string) string { return env[n] })
			if (err == nil) != tc.valid {
				t.Fatal("key/flag validity mismatch")
			}
			if err == nil && tc.flag == "true" && len(cfg.TokenAdminProxyHMACKey) != 32 {
				t.Fatal("key not bound to config")
			}
		})
	}
}
