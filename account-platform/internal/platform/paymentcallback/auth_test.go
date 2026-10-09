package paymentcallback

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAuthenticatorBindsProviderIdentityIntoShortLivedToken(t *testing.T) {
	auth, err := NewAuthenticator(AuthConfig{
		Clients:    []ClientConfig{{ClientID: "h3c-mall", ClientSecret: strings.Repeat("s", 32), ProviderID: "h3c_mall", ProviderVersion: "h3c-mall-v1", MerchantID: "pinvou"}},
		SigningKey: []byte(strings.Repeat("k", 32)), KeyID: "payment-callback-v1", TokenTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	auth.now = func() time.Time { return now }
	token, err := auth.Issue("h3c-mall", strings.Repeat("s", 32), Scope)
	if err != nil || token.TokenType != "Bearer" || token.ExpiresIn != 300 {
		t.Fatalf("unexpected token: %#v %v", token, err)
	}
	principal, err := auth.VerifyBearer("Bearer " + token.AccessToken)
	if err != nil || principal.ProviderID != "h3c_mall" || principal.ProviderVersion != "h3c-mall-v1" || principal.MerchantID != "pinvou" {
		t.Fatalf("unexpected principal: %#v %v", principal, err)
	}
	auth.now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := auth.VerifyBearer("Bearer " + token.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token was accepted")
	}
}

func TestAuthenticatorRejectsUnboundOrWeakClient(t *testing.T) {
	if _, err := NewAuthenticator(AuthConfig{Clients: []ClientConfig{{ClientID: "h3c", ClientSecret: "short"}}, SigningKey: []byte("short"), KeyID: "v1"}); err == nil {
		t.Fatal("weak callback authentication configuration was accepted")
	}
	auth, err := NewAuthenticator(AuthConfig{
		Clients:    []ClientConfig{{ClientID: "h3c", ClientSecret: strings.Repeat("s", 32), ProviderID: "h3c_mall", ProviderVersion: "h3c-mall-v1", MerchantID: "pinvou"}},
		SigningKey: []byte(strings.Repeat("k", 32)), KeyID: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, issueErr := auth.Issue("other", strings.Repeat("s", 32), Scope); !errors.Is(issueErr, ErrUnauthorized) {
		t.Fatal("unknown callback client was accepted")
	}
}
