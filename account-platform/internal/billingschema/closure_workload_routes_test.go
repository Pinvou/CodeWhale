package billingschema

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/go-jose/go-jose/v4"
)

func TestClosureRegisteredWorkloadScopes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "controlled", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "closure-fixture")
	for _, route := range []struct {
		name, method, path, permission string
		handler                        func() http.Handler
	}{
		{"account-state", "GET", "/internal/v1/users/00000000-0000-4000-8000-000000000001/account-closure-state", "internal.account.closure.read", func() http.Handler { return account.NewService(nil, nil).Handler(nil) }},
		{"billing-assess", "GET", "/internal/v1/users/00000000-0000-4000-8000-000000000001/account-closure-assessment", "internal.billing.account-closure.assess", func() http.Handler { return billing.NewService(nil).Handler("") }},
		{"billing-isolate", "POST", "/internal/v1/users/00000000-0000-4000-8000-000000000001/account-closure-grants/isolate", "internal.billing.account-closure.isolate", func() http.Handler { return billing.NewService(nil).Handler("") }},
		{"token-assess", "GET", "/internal/v1/users/00000000-0000-4000-8000-000000000001/account-closure-assessment", "internal.token.account-closure.assess", func() http.Handler {
			return token.NewService(nil, nil, nil).WithManagement(nil, nil, token.ProviderURLPolicy{}, "", nil).Handler(nil)
		}},
		{"token-clear", "DELETE", "/internal/v1/users/00000000-0000-4000-8000-000000000001/replay-payloads", "internal.token.account-closure.replay-clear", func() http.Handler {
			return token.NewService(nil, nil, nil).WithManagement(nil, nil, token.ProviderURLPolicy{}, "", nil).Handler(nil)
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			rules, _ := json.Marshal([]map[string]any{{"subject": "allowed", "service": "controlled-authority", "permissions": []string{route.permission}}, {"subject": "wrong-scope", "service": "controlled-authority", "permissions": []string{"internal.authorization.check"}}})
			t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
			handler := route.handler()
			for _, scenario := range []struct {
				subject string
				status  int
			}{{"allowed", 503}, {"wrong-scope", 403}, {"unregistered", 403}, {"", 401}} {
				t.Run(scenario.subject, func(t *testing.T) {
					request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
					if scenario.subject != "" {
						signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
						if e != nil {
							t.Fatal(e)
						}
						claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "closure-fixture", "sub": scenario.subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
						signed, e := signer.Sign(claims)
						if e != nil {
							t.Fatal(e)
						}
						raw, e := signed.CompactSerialize()
						if e != nil {
							t.Fatal(e)
						}
						request.Header.Set("Authorization", "Bearer "+raw)
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != scenario.status {
						t.Fatalf("status=%d expected=%d", response.Code, scenario.status)
					}
				})
			}
		})
	}
}
