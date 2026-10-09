package main

import "testing"

func TestSystemProbeConfigurationReader(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		count     int
		bad       bool
	}{
		{"empty", "", 0, false},
		{"installed", `[{"key":"newapi","kind":"http","url":"https://newapi:8444/api/status","json_boolean_field":"success"},{"key":"relay","kind":"relay","url":"https://relay:443/metrics"},{"key":"portal","kind":"local"}]`, 3, false},
		{"malformed", `[`, 0, true},
		{"unknown_kind", `[{"key":"bad","kind":"future","url":"https://example.test"}]`, 0, true},
		{"duplicate", `[{"key":"a","kind":"local"},{"key":" a ","kind":"local"}]`, 0, true},
		{"credentials", `[{"key":"bad","kind":"http","url":"https://user:password@example.test"}]`, 0, true},
		{"fragment", `[{"key":"bad","kind":"http","url":"https://example.test/#alias"}]`, 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			probes, err := loadSystemProbes(c.raw)
			if (err != nil) != c.bad || (!c.bad && len(probes) != c.count) {
				t.Fatalf("count=%d error=%v", len(probes), err)
			}
			if c.name == "installed" && (probes[0].JSONBooleanField != "success" || probes[0].Target.String() != "https://newapi:8444/api/status") {
				t.Fatal("configured status semantics changed")
			}
		})
	}
}

func TestSystemProbeConfigEntrypointPreservesOIDC(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example", "PORTAL_SYSTEM_PROBE_CA_FILE": "/etc/pinvou/probe-ca.pem", "SYSTEM_PROBES_JSON": `[{"key":"newapi","kind":"http","url":"https://newapi:8444/api/status","json_boolean_field":"success"}]`}
	cfg, err := loadPortalConfig("production", func(name string) string { return env[name] })
	if err != nil || cfg.SystemProbeCAFile != env["PORTAL_SYSTEM_PROBE_CA_FILE"] || len(cfg.SystemProbes) != 1 || cfg.PrivateCAFile != "" || cfg.TokenURL.String() != env["PORTAL_OIDC_TOKEN_URL"] || cfg.IssuerURL.String() != env["PORTAL_OIDC_ISSUER"] || cfg.JWKSURL.String() != env["PORTAL_OIDC_JWKS_URL"] {
		t.Fatal("probe config contaminated canonical OIDC", err)
	}
	env["SYSTEM_PROBES_JSON"] = "["
	if _, err := loadPortalConfig("production", func(name string) string { return env[name] }); err == nil {
		t.Fatal("invalid probe configuration accepted")
	}
}
