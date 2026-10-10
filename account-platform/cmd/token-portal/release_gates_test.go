package main

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestPortalReleaseGateEnvironmentIsStrictAndClosed(t *testing.T) {
	fields := map[string]string{"PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED": "AdminModelTokenIAEnabled", "PORTAL_ADMIN_TOKEN_RESOURCE_FORMS_ENABLED": "AdminTokenResourceFormsEnabled", "PORTAL_ADMIN_MODEL_OPERATIONS_DETAILS_ENABLED": "AdminModelOperationsDetailsEnabled", "PORTAL_ADMIN_MODEL_DETAILS_ENABLED": "AdminModelDetailsEnabled", "PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED": "AdminModelPricingEditEnabled", "PORTAL_ADMIN_MODEL_TOKEN_ACTIONS_ENABLED": "AdminModelTokenActionsEnabled", "PORTAL_ADMIN_MODEL_OPERATIONS_OVERVIEW_ENABLED": "AdminModelOperationsOverviewEnabled", "PORTAL_ADMIN_MODEL_TOKEN_PAGE_OVERVIEWS_ENABLED": "AdminModelTokenPageOverviewsEnabled", "PORTAL_ADMIN_TOKEN_DETAILS_ENABLED": "AdminTokenDetailsEnabled"}
	for name, field := range fields {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example", "TOKEN_ADMIN_PAGINATION_CURSOR_HMAC_KEY_BASE64": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("p", 32)))}
			get := func(k string) string { return env[k] }
			cfg, e := loadPortalConfig("test", get)
			if e != nil || reflect.ValueOf(cfg).FieldByName(field).Bool() {
				t.Fatal("gate default not closed")
			}
			env[name] = "true"
			cfg, e = loadPortalConfig("test", get)
			if e != nil || !reflect.ValueOf(cfg).FieldByName(field).Bool() {
				t.Fatal("gate not wired")
			}
			env[name] = "not-boolean"
			if _, e = loadPortalConfig("test", get); e == nil {
				t.Fatal("malformed gate accepted")
			}
		})
	}
}
