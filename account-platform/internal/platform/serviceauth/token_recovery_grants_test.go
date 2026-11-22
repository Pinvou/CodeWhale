package serviceauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
)

// Exercise the installed JWT subject mapping and Require boundary, with a
// private-CA JWKS and controlled issuer. The owner response has no side effect;
// this is not a live recovery, database or deployment acceptance test.
func TestTokenRecoveryWorkloadGrantRealSignedMiddleware(t *testing.T) {
	raw, err := os.ReadFile("../../../deploy/production/workload-token-rules.json.template")
	if err != nil {
		t.Fatal(err)
	}
	var original []rule
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	const caller = "account-approval-recovery"
	subject := func(service string) string { return "public-token-recovery:" + service }
	for i := range original {
		if original[i].Service == caller && original[i].Subject != "__APPROVAL_RECOVERY_WORKLOAD_SUBJECT__" {
			t.Fatal("production recovery grant uses the wrong projected workload subject")
		}
		original[i].Subject = subject(original[i].Service)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server, ca := trustTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/certs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "token-recovery-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	trustTestEnvironment(t, server.URL, ca)
	const audience = "pinvou-account-platform-internal"
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	issuer := server.URL + "/realms/workload"
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "token-recovery-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range []struct{ permission, path string }{
		{"internal.token.approval-effect.read", "/internal/v1/approval-effects/:approval_request_id"},
		{"internal.token.approval-result.read", "/internal/v1/approval-recovery/results/:approval_request_id"},
	} {
		t.Run(binding.permission, func(t *testing.T) {
			for _, scenario := range []struct {
				name string
				want int
			}{{"allowed", 204}, {"missing-exact-scope", 403}, {"borrowed-billing-subject", 403}, {"borrowed-executor-subject", 403}, {"unmapped-subject", 403}, {"wrong-issuer", 401}, {"wrong-audience", 401}, {"static-token", 401}} {
				t.Run(scenario.name, func(t *testing.T) {
					rules := make([]rule, len(original))
					for i := range original {
						rules[i] = original[i]
						rules[i].Permissions = append([]string(nil), original[i].Permissions...)
						if scenario.name == "missing-exact-scope" && rules[i].Service == caller {
							permissions := rules[i].Permissions[:0]
							for _, permission := range rules[i].Permissions {
								if permission != binding.permission {
									permissions = append(permissions, permission)
								}
							}
							rules[i].Permissions = permissions
						}
					}
					encoded, err := json.Marshal(rules)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(encoded))
					tokenSubject, tokenIssuer, tokenAudience := subject(caller), issuer, audience
					switch scenario.name {
					case "borrowed-billing-subject":
						tokenSubject = subject("billing-service")
					case "borrowed-executor-subject":
						tokenSubject = subject("token-command-executor")
					case "unmapped-subject":
						tokenSubject = subject("unmapped")
					case "wrong-issuer":
						tokenIssuer += "-other"
					case "wrong-audience":
						tokenAudience += "-other"
					}
					claims, err := json.Marshal(map[string]any{"sub": tokenSubject, "iss": tokenIssuer, "aud": tokenAudience, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
					if err != nil {
						t.Fatal(err)
					}
					signed, err := signer.Sign(claims)
					if err != nil {
						t.Fatal(err)
					}
					token, err := signed.CompactSerialize()
					if err != nil {
						t.Fatal(err)
					}
					if scenario.name == "static-token" {
						token = "public-static-fixture"
					}
					reached := false
					router := gin.New()
					router.Use(GinMiddleware("public-static-fixture"))
					router.GET(binding.path, Require(binding.permission), func(c *gin.Context) {
						reached = true
						identity, ok := FromContext(c.Request.Context())
						if !ok || identity.Service != caller || identity.Subject != subject(caller) || identity.Permission != binding.permission {
							t.Error("wrong authenticated recovery caller or scope reached the owner boundary")
						}
						c.Status(http.StatusNoContent)
					})
					request := httptest.NewRequest(http.MethodGet, strings.Replace(binding.path, ":approval_request_id", "10000000-0000-4000-8000-000000000001", 1), nil)
					request.Header.Set("Authorization", "Bearer "+token)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					if response.Code != scenario.want || reached != (scenario.want == http.StatusNoContent) {
						t.Fatalf("status=%d reached=%v, expected=%d", response.Code, reached, scenario.want)
					}
				})
			}
		})
	}
}
