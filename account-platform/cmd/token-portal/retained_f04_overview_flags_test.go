package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestRetainedF04OverviewEntrypointFlags(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example"}
	env["TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("f", 32)))
	flags := map[string]string{"PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED": "AdminModelOperationsOverviewEnabled", "PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED": "AdminModelTokenPageOverviewsEnabled"}
	for name, field := range flags {
		t.Run(name, func(t *testing.T) {
			get := func(key string) string { return env[key] }
			for _, value := range []string{"", "false", "true"} {
				env[name] = value
				cfg, err := loadPortalConfig("production", get)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				var decoded map[string]any
				if err = json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if (decoded[field] == true) != (value == "true") {
					t.Fatalf("flag %s was not consumed", name)
				}
			}
			env[name] = "not-a-boolean"
			if _, err := loadPortalConfig("production", get); err == nil {
				t.Fatal("invalid overview flag accepted")
			}
			delete(env, name)
		})
	}
}

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
