package main

import (
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

func TestPortalAdmissionEnvironmentMatchesApprovedLimits(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "http://portal.example", "PORTAL_OIDC_ISSUER": "http://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "http://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "http://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "http://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example"}
	get := func(k string) string { return env[k] }
	cfg, err := loadPortalConfig("test", get)
	want := portal.AdmissionLimits{OperationsPerSecond: 100, OperationBurst: 200, FlowCapacity: 65536, HandoffCapacity: 8192}
	if err != nil || cfg.AdmissionLimits != want {
		t.Fatal("approved default is not actual CLI configuration", cfg.AdmissionLimits, err)
	}
	for k, v := range map[string]string{"PORTAL_OPERATION_RPS": "250", "PORTAL_OPERATION_BURST": "500", "PORTAL_LOGIN_FLOW_CAPACITY": "1234", "PORTAL_SESSION_HANDOFF_CAPACITY": "456"} {
		env[k] = v
	}
	cfg, err = loadPortalConfig("test", get)
	if err != nil || cfg.AdmissionLimits != (portal.AdmissionLimits{OperationsPerSecond: 250, OperationBurst: 500, FlowCapacity: 1234, HandoffCapacity: 456}) {
		t.Fatal("site limits not parsed by CLI", cfg.AdmissionLimits, err)
	}
	for _, name := range []string{"PORTAL_OPERATION_RPS", "PORTAL_OPERATION_BURST", "PORTAL_LOGIN_FLOW_CAPACITY", "PORTAL_SESSION_HANDOFF_CAPACITY"} {
		old := env[name]
		for _, bad := range []string{"0", "-1", "1.5", "controlled-private-input", "100000000000000000000000000"} {
			env[name] = bad
			if _, err := loadPortalConfig("test", get); err == nil || strings.Contains(err.Error(), bad) {
				t.Fatal("invalid site limit accepted or exposed raw input", name, err)
			}
		}
		env[name] = old
	}
}
