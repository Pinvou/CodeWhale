package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
)

func TestPortalEntrypointUsesCanonicalEndpointsAndMountedHandoffSecret(t *testing.T) {
	secret := strings.Repeat("test-only-value-", 3)
	path := filepath.Join(t.TempDir(), "handoff-secret")
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PORTAL_PUBLIC_URL": "https://portal.example", "PORTAL_OIDC_ISSUER": "https://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "https://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "https://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "https://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example", "PORTAL_SESSION_HANDOFF": "true", "PORTAL_HANDOFF_OIDC_CLIENT_ID": "portal-handoff", "PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE": path}
	get := func(name string) string { return env[name] }
	cfg, err := loadPortalConfig("production", get)
	if err != nil || cfg.HandoffClientSecret != secret || cfg.SessionHandoffTTL.Seconds() != 30 || !cfg.SecureCookies {
		t.Fatal("canonical configuration not wired")
	}
	store, err := portal.NewRedisStore("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := portal.NewService(cfg, store); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PORTAL_OIDC_ISSUER", "PORTAL_OIDC_JWKS_URL", "PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE", "BILLING_HTTP_URL"} {
		original := env[name]
		env[name] = ""
		if _, err := loadPortalConfig("production", get); err == nil {
			t.Fatal("required canonical input accepted empty")
		}
		env[name] = original
	}
	env["PORTAL_SESSION_HANDOFF"] = "not-a-bool"
	if _, err := loadPortalConfig("production", get); err == nil {
		t.Fatal("invalid enable value accepted")
	}
}

func TestHandoffSecretReaderBoundsAndSafeErrors(t *testing.T) {
	for _, body := range []string{"short", strings.Repeat("x", 4097), strings.Repeat("x", 32) + "\nembedded", strings.Repeat("x", 32) + "\x00"} {
		path := filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readHandoffSecret(path, true); err == nil || strings.Contains(err.Error(), body) {
			t.Fatal("invalid secret accepted or exposed")
		}
	}
	if _, err := readHandoffSecret("relative", true); err == nil {
		t.Fatal("relative secret path accepted")
	}
	if _, err := readHandoffSecret(t.TempDir(), true); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := readHandoffSecret("", true); err == nil {
		t.Fatal("required file missing")
	}
	if value, err := readHandoffSecret("", false); err != nil || value != "" {
		t.Fatal("disabled handoff requires a secret")
	}
}

func TestPortalTrustedProxyEnvironmentIsExplicitAndValidated(t *testing.T) {
	env := map[string]string{"PORTAL_PUBLIC_URL": "http://portal.example", "PORTAL_OIDC_ISSUER": "http://identity.example/realm", "PORTAL_OIDC_JWKS_URL": "http://identity.example/keys", "PORTAL_OIDC_AUTHORIZATION_URL": "http://identity.example/auth", "PORTAL_OIDC_TOKEN_URL": "http://identity.example/token", "PORTAL_OIDC_CLIENT_ID": "portal", "ACCOUNT_HTTP_URL": "http://account.example", "TOKEN_GATEWAY_HTTP_URL": "http://token.example", "BILLING_HTTP_URL": "http://billing.example"}
	get := func(name string) string { return env[name] }
	cfg, err := loadPortalConfig("test", get)
	if err != nil || len(cfg.TrustedProxyIPs) != 0 {
		t.Fatal("default environment must not trust proxies")
	}
	env["PORTAL_TRUSTED_PROXY_IPS"] = "192.0.2.45,2001:db8::45"
	cfg, err = loadPortalConfig("test", get)
	if err != nil || len(cfg.TrustedProxyIPs) != 2 || cfg.TrustedProxyIPs[1].String() != "2001:db8::45" {
		t.Fatal("exact proxy configuration not wired")
	}
	for _, bad := range []string{"*", "192.0.2.0/24", "edge.example", "192.0.2.45,", "fe80::1%eth0"} {
		env["PORTAL_TRUSTED_PROXY_IPS"] = bad
		if _, err := loadPortalConfig("test", get); err == nil {
			t.Fatal("invalid proxy configuration accepted at startup")
		}
	}
}
