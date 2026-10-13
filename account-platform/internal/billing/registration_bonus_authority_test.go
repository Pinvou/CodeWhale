package billing

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
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/go-jose/go-jose/v4"
)

func TestRegistrationBonusRegisteredWorkloadScopes(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "controlled", Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	caDirectory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(caDirectory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: jwks.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := jwks.URL + "/realms/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", jwks.URL)
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "bonus-authority")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"account","service":"account-service","permissions":["internal.billing.registration-bonus.provision"]},{"subject":"billing","service":"billing-service","permissions":["internal.registration-bonus-eligibility.read"]},{"subject":"wrong-scope","service":"account-service","permissions":["internal.billing.wallet.ensure"]}]`)
	for _, endpoint := range []struct {
		name, method, path, subject string
		handler                     http.Handler
	}{
		{"billing", "POST", "/internal/v1/registration-bonus-grants", "account", NewService(nil).Handler("")},
		{"account", "GET", "/internal/v1/registration-bonus-eligibility/readiness", "billing", account.NewService(nil, nil).Handler(nil)},
	} {
		for _, scenario := range []struct {
			subject string
			status  int
		}{{endpoint.subject, 503}, {"wrong-scope", 403}, {"unregistered", 403}, {"", 401}} {
			t.Run(endpoint.name+"/"+scenario.subject, func(t *testing.T) {
				request := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(`{}`))
				if scenario.subject != "" {
					signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
					if err != nil {
						t.Fatal(err)
					}
					claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "bonus-authority", "sub": scenario.subject, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
					signed, err := signer.Sign(claims)
					if err != nil {
						t.Fatal(err)
					}
					token, err := signed.CompactSerialize()
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+token)
				}
				response := httptest.NewRecorder()
				endpoint.handler.ServeHTTP(response, request)
				if response.Code != scenario.status {
					t.Fatalf("scope boundary: %d %s", response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestRegistrationBonusReadinessRefreshesProjectedIdentityOverTLS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var router http.Handler
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jwks" {
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "controlled", Algorithm: "RS256", Use: "sig"}}})
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	caDirectory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(caDirectory, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := server.URL + "/realms/workload"
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", server.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "account-service")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", `[{"subject":"billing-first","service":"billing-service","permissions":["internal.registration-bonus-eligibility.read"]},{"subject":"billing-refreshed","service":"billing-service","permissions":["internal.registration-bonus-eligibility.read"]},{"subject":"wrong-scope","service":"billing-service","permissions":["internal.authorization.check"]}]`)
	seen := []string{}
	var seenMu sync.Mutex
	ginRouter := httpserver.NewRouter()
	ginRouter.Use(serviceauth.GinMiddleware(""))
	ginRouter.GET("/internal/v1/registration-bonus-eligibility/readiness", serviceauth.Require("internal.registration-bonus-eligibility.read"), httpserver.AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := serviceauth.FromContext(r.Context())
		if !ok || identity.Service != "billing-service" {
			t.Error("unverified workload")
		}
		seenMu.Lock()
		seen = append(seen, identity.Subject)
		seenMu.Unlock()
		now := time.Now().UTC()
		json.NewEncoder(w).Encode(RegistrationBonusEligibilityReadiness{Status: "READY", FingerprintKeyID: "controlled-key", CompletedAt: &now})
	}))
	router = ginRouter
	tokenFile := filepath.Join(t.TempDir(), "projected.jwt")
	writeToken := func(subject string) {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "controlled"))
		if err != nil {
			t.Fatal(err)
		}
		claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "account-service", "sub": subject, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()})
		signed, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		token, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
	}
	verifier, err := NewHTTPRegistrationBonusEligibilityVerifier(server.URL, "file:"+tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	verifier.client, err = serviceauth.NewWorkloadHTTPClient(ca)
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"billing-first", "billing-refreshed"} {
		writeToken(subject)
		if err := verifier.RequireReady(context.Background()); err != nil {
			t.Fatal("verified readiness request failed")
		}
	}
	seenMu.Lock()
	correctSubjects := len(seen) == 2 && seen[0] == "billing-first" && seen[1] == "billing-refreshed"
	seenMu.Unlock()
	if !correctSubjects {
		t.Fatal("readiness cached a stale projected credential")
	}
	writeToken("wrong-scope")
	if err := verifier.RequireReady(context.Background()); err == nil {
		t.Fatal("readiness accepted a workload without its exact scope")
	}
	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 {
		t.Fatal("unauthorized readiness reached authority")
	}
}
