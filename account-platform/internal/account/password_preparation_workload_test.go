package account

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/go-jose/go-jose/v4"
)

// Exercise Identity's real HTTP writer and Account's registered claim route
// with signed workload credentials. The intentionally invalid claim token is
// rejected before any SQL mutation, while auth failures remain distinguishable.
func TestPasswordPreparationExactWorkloadRoute(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "claim-fixture", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600) != nil {
		t.Fatal("owned workload CA write failed")
	}
	issuer, audience := jwks.URL+"/workload", "account-claim-fixture"
	t.Setenv("APP_ENV", "production")
	t.Setenv("GIN_MODE", "release")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"identity-exact","service":"identity-service","permissions":["internal.auth.password-preparation.claim"]},{"subject":"identity-other-scope","service":"identity-service","permissions":["internal.auth.sms.verify"]},{"subject":"billing","service":"billing-service","permissions":["internal.authorization.check"]}]`)
	signer, err := NewRegistrationTokenSigner([]byte(strings.Repeat("c", 32)))
	if err != nil {
		t.Fatal(err)
	}
	handler := NewService(nil, nil).WithMobileAuth(&MobileAuthService{tokens: signer}, "").Handler(nil)
	var observedCode atomic.Value
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorded := httptest.NewRecorder()
		handler.ServeHTTP(recorded, r)
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(recorded.Body.Bytes(), &body)
		observedCode.Store(body.Code)
		for key, values := range recorded.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	}))
	defer accountServer.Close()
	tokenFile := filepath.Join(t.TempDir(), "projected-workload.jwt")
	verifier, err := identityadapter.NewHTTPPasswordPreparationVerifier(accountServer.URL, "file:"+tokenFile, "")
	if err != nil {
		t.Fatal("real Identity writer configuration failed")
	}
	for _, scenario := range []struct {
		subject, issuer, audience, code string
		allowed                         bool
	}{
		{"identity-exact", issuer, audience, "PASSWORD_PREPARATION_INVALID", true},
		{"identity-other-scope", issuer, audience, "SERVICE_AUTH_FORBIDDEN", false},
		{"billing", issuer, audience, "SERVICE_AUTH_FORBIDDEN", false},
		{"unregistered", issuer, audience, "SERVICE_AUTH_FORBIDDEN", false},
		{"identity-exact", issuer + "/wrong", audience, "SERVICE_AUTH_REQUIRED", false},
		{"identity-exact", issuer, "wrong-audience", "SERVICE_AUTH_REQUIRED", false},
	} {
		t.Run(scenario.subject+"/"+scenario.code, func(t *testing.T) {
			jwtSigner, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "claim-fixture"))
			if err != nil {
				t.Fatal(err)
			}
			claims, _ := json.Marshal(map[string]any{"iss": scenario.issuer, "aud": scenario.audience, "sub": scenario.subject, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix()})
			signed, err := jwtSigner.Sign(claims)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			if os.WriteFile(tokenFile, []byte(raw), 0600) != nil {
				t.Fatal("owned credential projection failed")
			}
			_, err = verifier.Claim(context.Background(), passwordpreparation.Claim{Token: "intentionally-invalid-fixture-claim"})
			if observedCode.Load() != scenario.code {
				t.Fatalf("real claim route code=%v expected=%s", observedCode.Load(), scenario.code)
			}
			if scenario.allowed && !errors.Is(err, identityadapter.ErrPasswordPreparation) {
				t.Fatal("exact permission did not reach domain claim validation")
			}
			if !scenario.allowed && !errors.Is(err, identityadapter.ErrPasswordPreparationUnavailable) {
				t.Fatal("unauthorized caller was confused with a domain claim refusal")
			}
		})
	}
}
