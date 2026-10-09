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

func TestF06OrganizationLifecycleRegisteredWorkloadScope(t *testing.T) {
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
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account","service":"account-service","permissions":["internal.token.organization.lifecycle","internal.token.organization-unit.lifecycle"]},{"subject":"wrong-scope","service":"account-service","permissions":["internal.authorization.check"]}]`)
	handler := (&Service{managementEnabled: true}).Handler(nil)
	for _, path := range []string{"/internal/v1/organizations/00000000-0000-4000-8000-000000000001/lifecycle", "/internal/v1/organizations/00000000-0000-4000-8000-000000000001/units/00000000-0000-4000-8000-000000000002/lifecycle"} {
		for subject, status := range map[string]int{"account": 503, "unknown-subject": 403, "wrong-scope": 403, "": 401} {
			request := httptest.NewRequest("PUT", path, strings.NewReader(`{"status":"ACTIVE","account_version":1}`))
			if strings.Contains(path, "/units/") {
				request = httptest.NewRequest("PUT", path, strings.NewReader(`{"status":"ACTIVE","structure_version":1}`))
			}
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
}
