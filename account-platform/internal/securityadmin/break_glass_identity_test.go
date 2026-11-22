package securityadmin

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func signedBreakGlassCredential(t *testing.T, privateKey ed25519.PrivateKey, identity BreakGlassIdentity) string {
	t.Helper()
	payload, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(breakGlassCredentialEnvelope{
		Payload:   base64.RawURLEncoding.EncodeToString(payload),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(envelope)
}

func TestBreakGlassCredentialBindsTrustedOperatorAndTarget(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	verifier, err := NewBreakGlassCredentialVerifier(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		t.Fatal(err)
	}
	verifier.now = func() time.Time { return now }
	raw := signedBreakGlassCredential(t, privateKey, BreakGlassIdentity{CredentialID: "incident-credential-001", Subject: "cluster:prod:security-operator", TargetUsername: "recovery-admin", Audience: breakGlassAudience, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(5 * time.Minute)})
	identity, err := verifier.Verify(raw)
	if err != nil || identity.Subject != "cluster:prod:security-operator" || identity.TargetUsername != "recovery-admin" {
		t.Fatalf("unexpected verified identity %#v, %v", identity, err)
	}

	var envelope breakGlassCredentialEnvelope
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		t.Fatal("invalid test envelope")
	}
	envelope.Payload = base64.RawURLEncoding.EncodeToString([]byte(`{"credential_id":"forged","subject":"attacker"}`))
	forged, _ := json.Marshal(envelope)
	if _, err := verifier.Verify(string(forged)); !errors.Is(err, ErrBreakGlassIdentityInvalid) {
		t.Fatalf("tampered identity must fail, got %v", err)
	}
}

func TestBreakGlassCredentialRejectsExpiredOrOverlongGrant(t *testing.T) {
	publicKey, privateKey, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	verifier, _ := NewBreakGlassCredentialVerifier(base64.RawURLEncoding.EncodeToString(publicKey))
	verifier.now = func() time.Time { return now }
	for _, identity := range []BreakGlassIdentity{
		{CredentialID: "expired-credential", Subject: "operator", TargetUsername: "admin", Audience: breakGlassAudience, IssuedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(-time.Minute)},
		{CredentialID: "overlong-credential", Subject: "operator", TargetUsername: "admin", Audience: breakGlassAudience, IssuedAt: now, ExpiresAt: now.Add(16 * time.Minute)},
	} {
		if _, err := verifier.Verify(signedBreakGlassCredential(t, privateKey, identity)); !errors.Is(err, ErrBreakGlassIdentityInvalid) {
			t.Fatalf("unsafe credential must fail, got %v", err)
		}
	}
}
