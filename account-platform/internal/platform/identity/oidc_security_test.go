package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func TestOIDCVerifierAcceptsOnlyAccessTokens(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "fixture", Use: "sig", Algorithm: "RS256"}}})
	}))
	defer server.Close()
	verify, err := NewOIDCVerifyFuncWithJWKS(context.Background(), server.URL, server.URL, "fixture-client", "test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tokenType := range []string{"Bearer", "ID", "Refresh", ""} {
		t.Run("type="+tokenType, func(t *testing.T) {
			claims, err := json.Marshal(map[string]any{"iss": server.URL, "aud": "fixture-client", "sub": "fixture-user", "sid": "fixture-session", "exp": time.Now().Add(time.Minute).Unix(), "typ": tokenType})
			if err != nil {
				t.Fatal(err)
			}
			signed, err := signer.Sign(claims)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				t.Fatal(err)
			}
			principal, err := verify(context.Background(), raw)
			if tokenType != "Bearer" {
				if err == nil {
					t.Fatal("non-access token was accepted")
				}
				return
			}
			if err != nil || principal.Subject != "fixture-user" || !principal.AuthenticatedAt.IsZero() {
				t.Fatalf("access token without auth_time = %+v, %v", principal, err)
			}
		})
	}
}

func TestOIDCVerifierRejectsCredentialAndFragmentURLs(t *testing.T) {
	for _, raw := range []string{"https://user:fixture@example.test/keys", "https://example.test/keys#fragment"} {
		if _, err := NewOIDCVerifyFuncWithJWKS(context.Background(), "https://issuer.test", raw, "client", "test"); err == nil {
			t.Fatalf("unsafe JWKS URL accepted: %s", raw)
		}
		if _, err := NewOIDCVerifyFunc(context.Background(), raw, "client", "test"); err == nil {
			t.Fatalf("unsafe issuer URL accepted: %s", raw)
		}
	}
}

func TestOIDCSharedKeyFetchHasFiniteTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	verify, err := NewOIDCVerifyFuncWithJWKS(context.Background(), server.URL, server.URL, "client", "test")
	if err != nil {
		t.Fatal(err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := json.Marshal(map[string]any{"iss": server.URL, "aud": "client", "sub": "user", "sid": "session", "typ": "Bearer", "exp": time.Now().Add(time.Minute).Unix()})
	signed, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	started := time.Now()
	if _, err := verify(ctx, raw); err == nil {
		t.Fatal("unavailable key server was accepted")
	}
	if elapsed := time.Since(started); elapsed >= 7*time.Second {
		t.Fatalf("shared key fetch was not bounded independently: %s", elapsed)
	}
}
