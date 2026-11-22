package identityadapter

import (
	"context"
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

	"github.com/go-jose/go-jose/v4"
)

type closureDeleteDirectory struct {
	directoryFake
	deletes int
	subject string
}

func (d *closureDeleteDirectory) DeleteUser(_ context.Context, subject string) error {
	d.deletes++
	d.subject = subject
	return nil
}

func TestClosureDeleteRequiresRegisteredWorkloadAuthority(t *testing.T) {
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
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "identity-adapter")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account","service":"account-service","permissions":["internal.identity.user.delete"]},{"subject":"wrong-scope","service":"account-service","permissions":["internal.identity.user.create"]}]`)
	directory := &closureDeleteDirectory{}
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(vault, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := service.Handler()
	for _, scenario := range []struct {
		subject string
		status  int
	}{{"wrong-scope", 403}, {"unregistered", 403}, {"", 401}, {"account", 204}} {
		t.Run(scenario.subject, func(t *testing.T) {
			request := httptest.NewRequest("DELETE", "/internal/v1/users/controlled-user", nil)
			before := directory.deletes
			if scenario.subject != "" {
				signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
				if e != nil {
					t.Fatal(e)
				}
				claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "identity-adapter", "sub": scenario.subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
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
			if scenario.status == 204 {
				if directory.deletes != before+1 || directory.subject != "controlled-user" {
					t.Fatal("delete target changed")
				}
			} else if directory.deletes != before {
				t.Fatal("unauthorized identity deletion")
			}
		})
	}
}
