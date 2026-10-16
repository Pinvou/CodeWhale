package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func TestPersonalExecutionUsesCanonicalProductionSafetyGates(t *testing.T) {
	for _, environment := range []string{"production", "production-new", "production-eu", " Production-East "} {
		for _, gate := range []string{"signing key", "on-call schedule"} {
			t.Run(environment+"/"+gate, func(t *testing.T) {
				t.Setenv("CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64", "")
				t.Setenv("CLIENT_READINESS_ATTESTATION_KEY_ID", "controlled-readiness")
				t.Setenv("CLIENT_READINESS_ATTESTATION_TTL_SECONDS", "600")
				t.Setenv("TOKEN_GATEWAY_MAX_INPUT_TOKENS", "128000")
				t.Setenv("TOKEN_RECONCILIATION_INTERVAL_SECONDS", "15")
				t.Setenv("TOKEN_GATEWAY_ADAPTER", "CONTROLLED")
				t.Setenv("TOKEN_MANUAL_REVIEW_ONCALL_USER_IDS", "")
				t.Setenv("ACCOUNT_HTTP_URL", "https://account.invalid")
				t.Setenv("OIDC_ISSUER", "https://issuer.invalid")
				if gate == "on-call schedule" {
					t.Setenv("CLIENT_READINESS_ATTESTATION_PRIVATE_KEY_BASE64", base64.StdEncoding.EncodeToString(make([]byte, 32)))
				}
				err := installPersonalExecution(context.Background(), token.NewService(nil, nil, nil), &token.PostgresRepository{}, &token.HTTPBillingClient{}, token.NewHTTPUpstreamClient(), &token.PostgresDefaultRouteProvisioner{}, token.ProviderURLPolicy{}, environment, "controlled-service")
				if err == nil || (gate == "signing key" && !strings.Contains(err.Error(), "signing key is required")) || (gate == "on-call schedule" && !strings.Contains(err.Error(), "ON_CALL")) {
					t.Fatalf("environment=%q gate=%q did not enforce existing production requirement: %v", environment, gate, err)
				}
			})
		}
	}
}
