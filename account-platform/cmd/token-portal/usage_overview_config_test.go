package main

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestF05UsageOverviewFlagsCurrentLoader(t *testing.T) {
	for _, tc := range []struct {
		name, aggregate, page            string
		wantAggregate, wantPage, invalid bool
	}{
		{"default", "", "", false, false, false}, {"page", "false", "true", false, true, false},
		{"fallback", "true", "false", true, false, false}, {"bad-page", "true", "bad", false, false, true},
		{"bad-aggregate", "bad", "true", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{"PORTAL_PUBLIC_URL": "http://portal.example", "PORTAL_OIDC_ISSUER": "http://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "http://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "http://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "http://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example", "PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED": tc.aggregate, "PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED": tc.page}
			env["TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64"] = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32)))
			cfg, err := loadPortalConfig("test", func(name string) string { return env[name] })
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid overview boolean accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			value := reflect.ValueOf(cfg)
			for name, want := range map[string]bool{"AdminModelOperationsOverviewEnabled": tc.wantAggregate, "AdminModelTokenPageOverviewsEnabled": tc.wantPage} {
				field := value.FieldByName(name)
				if !field.IsValid() || field.Kind() != reflect.Bool || field.Bool() != want {
					t.Fatalf("overview flag %s not delivered", name)
				}
			}
		})
	}
}
