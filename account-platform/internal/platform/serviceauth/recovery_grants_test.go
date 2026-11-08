package serviceauth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v4"
)

// This is the installed workload authenticator/Require boundary, with a real
// private-CA TLS JWKS and locally signed tokens. The restricted handler has no
// financial side effect; this does not claim a complete recovery workflow.
func TestExpiredHoldRecoveryWorkloadGrantRealSignedMiddleware(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server, ca := trustTestTLS(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/certs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "public-recovery-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	trustTestEnvironment(t, server.URL, ca)
	const audience = "pinvou-account-platform-internal"
	issuer := server.URL + "/realms/workload"
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "public-recovery-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	sign := func(subject, tokenIssuer, tokenAudience string) string {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"iss": tokenIssuer, "aud": tokenAudience, "sub": subject, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(raw)
		if err != nil {
			t.Fatal(err)
		}
		token, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	subject := func(service string) string { return "public-fixture:" + service }
	for _, binding := range []struct{ receiver, caller, permission, path, other string }{
		{"token", "billing-service", "internal.token.hold-expiry.prove", "/internal/v1/holds/expiry-proof", "account-approval-recovery"},
	} {
		t.Run(binding.receiver+"-from-"+binding.caller+"-"+binding.permission, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "production", "workload-"+binding.receiver+"-rules.json.template"))
			if err != nil {
				t.Fatal(err)
			}
			var original []rule
			if err = json.Unmarshal(raw, &original); err != nil {
				t.Fatal(err)
			}
			for i := range original {
				if original[i].Service == binding.caller && original[i].Subject != "__BILLING_WORKLOAD_SUBJECT__" {
					t.Fatal("production grant is bound to the wrong subject placeholder")
				}
				original[i].Subject = subject(original[i].Service)
			}
			for _, scenario := range []struct {
				name string
				want int
			}{{"allowed", 204}, {"missing-exact-grant", 403}, {"other-mapped-caller", 403}, {"unmapped-subject", 403}, {"wrong-issuer", 401}, {"wrong-audience", 401}, {"static-token", 401}} {
				t.Run(scenario.name, func(t *testing.T) {
					rules := make([]rule, len(original))
					for i := range original {
						rules[i] = original[i]
						rules[i].Permissions = append([]string(nil), original[i].Permissions...)
					}
					if scenario.name == "missing-exact-grant" {
						found := false
						for i := range rules {
							if rules[i].Service == binding.caller {
								for j, p := range rules[i].Permissions {
									if p == binding.permission {
										rules[i].Permissions = append(rules[i].Permissions[:j], rules[i].Permissions[j+1:]...)
										found = true
										break
									}
								}
							}
						}
						if !found {
							t.Fatal("positive config lacks the exact grant to remove")
						}
					}
					encoded, err := json.Marshal(rules)
					if err != nil {
						t.Fatal(err)
					}
					t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(encoded))
					tokenSubject, tokenIssuer, tokenAudience := subject(binding.caller), issuer, audience
					switch scenario.name {
					case "other-mapped-caller":
						tokenSubject = subject(binding.other)
					case "unmapped-subject":
						tokenSubject = subject("unmapped")
					case "wrong-issuer":
						tokenIssuer = issuer + "-other"
					case "wrong-audience":
						tokenAudience = audience + "-other"
					}
					token := sign(tokenSubject, tokenIssuer, tokenAudience)
					if scenario.name == "static-token" {
						token = "public-static-fixture"
					}
					reached := false
					router := gin.New()
					router.Use(GinMiddleware("public-static-fixture"))
					router.POST(binding.path, Require(binding.permission), func(c *gin.Context) {
						reached = true
						identity, ok := FromContext(c.Request.Context())
						if !ok || identity.Service != binding.caller || identity.Subject != subject(binding.caller) || identity.Permission != binding.permission {
							t.Error("wrong authenticated caller or exact scope reached handler")
						}
						c.Status(http.StatusNoContent)
					})
					request := httptest.NewRequest(http.MethodPost, binding.path, strings.NewReader(`{}`))
					request.Header.Set("Authorization", "Bearer "+token)
					response := httptest.NewRecorder()
					router.ServeHTTP(response, request)
					if response.Code != scenario.want || reached != (scenario.want == http.StatusNoContent) {
						t.Fatalf("%s returned %d/handler=%v, expected %d", scenario.name, response.Code, reached, scenario.want)
					}
				})
			}
		})
	}
}
