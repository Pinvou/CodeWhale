package account

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A controlled issuer still exercises the real Account workload verifier and
// exact Token consumer permissions. No static-token identity fallback is used.
func configureTerminalRecoveryTokenIdentity(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "approval-executor",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()), "e": "AQAB",
		}}})
	}))
	t.Cleanup(issuer.Close)
	ca := filepath.Join(t.TempDir(), "issuer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	subject, audience := uuid.NewString(), "terminal-recovery-integration"
	rules, err := json.Marshal([]map[string]any{{"subject": subject, "service": "token-command-executor", "permissions": []string{"internal.approval-authorization.consume"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("APP_ENV", "production")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", ca)
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", issuer.URL)
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", audience)
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", issuer.URL+"/jwks")
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(rules))
	now := time.Now().UTC()
	token, err := signEntitlementFixtureJWT(key, map[string]any{"iss": issuer.URL, "sub": subject, "aud": audience, "exp": now.Add(10 * time.Minute).Unix(), "iat": now.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return token
}
