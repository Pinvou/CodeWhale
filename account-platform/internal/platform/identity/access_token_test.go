package identity

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestSignedAccessTokenBoundary(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/realms/pinvou/protocol/openid-connect/certs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
		}}})
	}))
	defer server.Close()
	issuer := server.URL + "/realms/pinvou"
	verify, err := NewOIDCVerifyFunc(context.Background(), issuer, "pinvou-desktop-dev", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name     string
		change   func(map[string]any)
		accepted bool
	}{
		{"access", func(map[string]any) {}, true},
		{"id", func(c map[string]any) { c["typ"] = "ID" }, false},
		{"refresh", func(c map[string]any) { c["typ"] = "Refresh" }, false},
		{"missing-type", func(c map[string]any) { delete(c, "typ") }, false},
		{"wrong-issuer", func(c map[string]any) { c["iss"] = "https://other.example.test" }, false},
		{"wrong-audience", func(c map[string]any) { c["aud"] = "other-client" }, false},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, false},
		{"missing-subject", func(c map[string]any) { delete(c, "sub") }, false},
		{"missing-session", func(c map[string]any) { delete(c, "sid") }, false},
		{"session-state-fallback", func(c map[string]any) { delete(c, "sid"); c["session_state"] = "test-session" }, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			claims := map[string]any{"iss": issuer, "aud": "pinvou-desktop-dev", "sub": "test-subject", "sid": "test-session", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()}
			scenario.change(claims)
			principal, err := verify(context.Background(), signAccessTestToken(t, key, claims))
			if (err == nil) != scenario.accepted {
				t.Fatalf("accepted=%v want=%v error=%v", err == nil, scenario.accepted, err)
			}
			if scenario.accepted && (principal.Issuer != issuer || principal.Subject != "test-subject" || principal.SessionID != "test-session") {
				t.Fatalf("unexpected verified principal: %+v", principal)
			}
		})
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify(context.Background(), signAccessTestToken(t, otherKey, map[string]any{"iss": issuer, "aud": "pinvou-desktop-dev", "sub": "test-subject", "sid": "test-session", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()})); err == nil {
		t.Fatal("a token signed by an untrusted key was accepted")
	}
}

func TestStalledJWKSFetchIsBoundedAndRecovers(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	firstCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			<-r.Context().Done()
			close(firstCanceled)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
		}}})
	}))
	defer server.Close()
	defer server.CloseClientConnections()
	issuer := server.URL + "/realms/pinvou"
	verify, err := NewOIDCVerifyFunc(context.Background(), issuer, "pinvou-desktop-dev", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	jwt := signAccessTestToken(t, key, map[string]any{"iss": issuer, "aud": "pinvou-desktop-dev", "sub": "test-subject", "sid": "test-session", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := verify(ctx, jwt); err == nil || ctx.Err() != nil || time.Since(started) > 7*time.Second {
		t.Fatalf("stalled fetch was not bounded independently of caller context: error=%v elapsed=%s", err, time.Since(started))
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("timed out JWKS HTTP request was not canceled")
	}
	if _, err := verify(ctx, jwt); err != nil {
		t.Fatalf("authentication did not recover after the stalled fetch: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("JWKS requests=%d want=2", requests.Load())
	}
}

func TestSeparateJWKSAddressRetainsPublicIssuer(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/internal-certs" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "test-key",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB",
		}}})
	}))
	defer server.Close()
	issuer := "https://public.example.test/realms/pinvou"
	verify, err := NewOIDCVerifyFunc(context.Background(), issuer, "desktop", "test", server.URL+"/internal-certs")
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{"iss": issuer, "aud": "desktop", "sub": "user", "sid": "session", "typ": "Bearer", "exp": time.Now().Add(time.Hour).Unix()}
	principal, err := verify(context.Background(), signAccessTestToken(t, key, claims))
	if err != nil || principal.Issuer != issuer || requests.Load() != 1 {
		t.Fatalf("public issuer with internal keys failed: principal=%+v requests=%d error=%v", principal, requests.Load(), err)
	}
	claims["iss"] = server.URL
	if _, err := verify(context.Background(), signAccessTestToken(t, key, claims)); err == nil {
		t.Fatal("internal key address was incorrectly accepted as issuer")
	}
}

func TestJWKSConfigurationFailsClosed(t *testing.T) {
	for _, address := range []string{"/relative", "ftp://keys.example.test/certs", "https://user:password@keys.example.test/certs", "https://keys.example.test/certs#fragment", "http://keys.example.test/certs"} {
		t.Run(address, func(t *testing.T) {
			if _, err := NewOIDCVerifyFunc(context.Background(), "https://public.example.test/realm", "desktop", "production", address); err == nil {
				t.Fatal("unsafe production JWKS configuration was accepted")
			}
		})
	}
}

func signAccessTestToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "RS256", "kid": "test-key", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	digest := sha256.Sum256([]byte(input))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}
