package main

import (
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/portal"
	"os"
	"testing"
)

func TestComposePortalEndpointsThroughCurrentLoader(t *testing.T) {
	p := os.Getenv("F18_COMPOSE_PORTAL_ENV")
	if p == "" {
		t.Skip("explicit immutable Compose environment evidence required")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var profiles map[string]map[string]string
	if json.Unmarshal(raw, &profiles) != nil || len(profiles) != 2 {
		t.Fatal("two Compose source profiles required")
	}
	store, err := portal.NewRedisStore("redis://127.0.0.1:1/0")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for name, env := range profiles {
		t.Run(name, func(t *testing.T) {
			cfg, err := loadPortalConfig(env["APP_ENV"], func(k string) string { return env[k] })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := portal.NewService(cfg, store); err != nil {
				t.Fatal(err)
			}
			if cfg.IssuerURL.String() != env["PORTAL_OIDC_ISSUER"] || cfg.JWKSURL.String() != env["PORTAL_OIDC_JWKS_URL"] {
				t.Fatal("producer reader mismatch")
			}
		})
	}
	for _, key := range []string{"PORTAL_OIDC_ISSUER", "PORTAL_OIDC_JWKS_URL"} {
		t.Run("missing_"+key, func(t *testing.T) {
			e := profiles["model-token"]
			prior := e[key]
			e[key] = ""
			defer func() { e[key] = prior }()
			if _, err := loadPortalConfig(e["APP_ENV"], func(k string) string { return e[k] }); err == nil {
				t.Fatal("missing endpoint accepted")
			}
		})
	}
}
