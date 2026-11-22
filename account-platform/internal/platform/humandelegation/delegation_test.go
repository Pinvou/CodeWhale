package humandelegation

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDelegationBindsEverySensitiveDimension(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := NewSigner("account-2026-08", privateKey)
	verifier, _ := NewVerifier(map[string]ed25519.PublicKey{"account-2026-08": publicKey})
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	signer.now, verifier.now = func() time.Time { return now }, func() time.Time { return now.Add(time.Minute) }
	organizationID, unitID := uuid.New(), uuid.New()
	claims := Claims{Audience: "billing-service", HumanUserID: uuid.New(), Permission: "org.quota.allocate", AuthorizationVersion: 7, OrganizationID: &organizationID, UnitID: &unitID, ResourceType: "member-quota", ResourceID: "grant-7", HTTPMethod: "POST", HTTPPath: "/internal/v1/member-quota-grants", RequestSHA256: RequestDigest([]byte(`{"amount":200}`))}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	expected := Expectation{Audience: claims.Audience, Permission: claims.Permission, OrganizationID: claims.OrganizationID, UnitID: claims.UnitID, ResourceType: claims.ResourceType, ResourceID: claims.ResourceID, HTTPMethod: claims.HTTPMethod, HTTPPath: claims.HTTPPath, RequestSHA256: claims.RequestSHA256}
	verified, err := verifier.Verify(token, expected)
	if err != nil || verified.HumanUserID != claims.HumanUserID || verified.AuthorizationVersion != 7 {
		t.Fatalf("unexpected verified delegation: %#v, %v", verified, err)
	}
	tampered := expected
	tampered.ResourceID = "grant-8"
	if _, err := verifier.Verify(token, tampered); !errors.Is(err, ErrInvalid) {
		t.Fatal("resource substitution must fail")
	}
	tampered = expected
	tampered.RequestSHA256 = strings.Repeat("0", 64)
	if _, err := verifier.Verify(token, tampered); !errors.Is(err, ErrInvalid) {
		t.Fatal("body substitution must fail")
	}
}

func TestDelegationRejectsTamperingAndExpiry(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(nil)
	signer, _ := NewSigner("account-current", privateKey)
	verifier, _ := NewVerifier(map[string]ed25519.PublicKey{"account-current": publicKey})
	now := time.Now().UTC()
	signer.now = func() time.Time { return now }
	claims := Claims{Audience: "token-gateway", HumanUserID: uuid.New(), Permission: "personal.model.invoke", AuthorizationVersion: 2, ResourceType: "model", ResourceID: "model-a", HTTPMethod: "POST", HTTPPath: "/internal/v1/model-invocations", RequestSHA256: RequestDigest(nil)}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	replacement := byte('A')
	if token[len(token)-1] == replacement {
		replacement = 'B'
	}
	broken := token[:len(token)-1] + string(replacement)
	if _, err := verifier.Verify(broken, Expectation{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("tampered token must fail")
	}
	verifier.now = func() time.Time { return now.Add(MaximumLifetime + time.Second) }
	if _, err := verifier.Verify(token, Expectation{Audience: claims.Audience, Permission: claims.Permission, ResourceType: claims.ResourceType, ResourceID: claims.ResourceID, HTTPMethod: claims.HTTPMethod, HTTPPath: claims.HTTPPath, RequestSHA256: claims.RequestSHA256}); !errors.Is(err, ErrInvalid) {
		t.Fatal("expired token must fail")
	}
}
