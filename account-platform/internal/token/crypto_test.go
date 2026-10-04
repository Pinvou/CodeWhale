package token

import (
	"bytes"
	"testing"
)

func TestLocalAESProviderRoundTripAndAADBinding(t *testing.T) {
	provider, err := NewLocalAESProvider("test-key", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte("provider-token-that-must-not-leak")
	ciphertext, err := provider.Encrypt(plaintext, []byte("credential-a"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}
	decoded, err := provider.Decrypt(ciphertext, []byte("credential-a"))
	if err != nil || !bytes.Equal(decoded, plaintext) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := provider.Decrypt(ciphertext, []byte("credential-b")); err == nil {
		t.Fatal("decrypt accepted different associated data")
	}
}

func TestProductionRejectsLocalSecretProvider(t *testing.T) {
	if _, err := NewSecretProvider("production-new", "local", "local-key", bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("production accepted local secret provider")
	}
}

func TestProviderURLPolicyBlocksSSRFAndProductionHTTP(t *testing.T) {
	development, err := NewProviderURLPolicy("development", []string{"mock-newapi"})
	if err != nil {
		t.Fatal(err)
	}
	if err := development.Validate("http://mock-newapi:8080"); err != nil {
		t.Fatalf("development mock URL rejected: %v", err)
	}
	for _, raw := range []string{"http://127.0.0.1:8080", "http://169.254.169.254/latest/meta-data", "http://mock-newapi:8080?target=internal", "http://user:pass@mock-newapi:8080"} {
		if err := development.Validate(raw); err == nil {
			t.Fatalf("unsafe provider URL accepted: %s", raw)
		}
	}
	production, err := NewProviderURLPolicy("production-new", []string{"newapi.internal"})
	if err != nil {
		t.Fatal(err)
	}
	if err := production.Validate("http://newapi.internal"); err == nil {
		t.Fatal("production accepted plaintext provider URL")
	}
	if err := production.Validate("https://newapi.internal"); err != nil {
		t.Fatalf("production HTTPS provider rejected: %v", err)
	}
}
