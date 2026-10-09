package token

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"github.com/go-jose/go-jose/v4"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestF04ExpiredHoldRegisteredRouteRequiresBillingWorkloadScope(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "controlled", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/realms/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "token-service")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"billing","service":"billing-service","permissions":["internal.token.hold-expiry.prove"]},{"subject":"wrong-service","service":"portal-service","permissions":["internal.token.hold-expiry.prove"]},{"subject":"wrong-scope","service":"billing-service","permissions":["internal.authorization.check"]}]`)
	handler := (&Service{managementEnabled: true}).Handler(nil)
	for subject, status := range map[string]int{"billing": 400, "wrong-service": 403, "wrong-scope": 403, "": 401} {
		request := httptest.NewRequest("POST", "/internal/v1/holds/expiry-proof", strings.NewReader(`{}`))
		if subject != "" {
			signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
			if e != nil {
				t.Fatal(e)
			}
			claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "token-service", "sub": subject, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
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
		if response.Code != status {
			t.Fatalf("registered authority %s: %d %s", subject, response.Code, response.Body.String())
		}
	}
}
