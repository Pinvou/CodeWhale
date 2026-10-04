package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if _, err := NewSecretProvider(SecretProviderConfig{Environment: "production-new", Mode: "local", KeyID: "local-key", LocalKey: bytes.Repeat([]byte{1}, 32)}); err == nil {
		t.Fatal("production accepted local secret provider")
	}
}

func TestRemoteEnvelopeProviderUsesAuthenticatedAADBoundRequests(t *testing.T) {
	secret := "remote-provider-token-that-is-long-enough"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Fatal("remote provider request was not authenticated")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["key_id"] != "kms-key-v1" || body["associated_data_b64"] != base64.StdEncoding.EncodeToString([]byte("credential-id")) {
			t.Fatalf("request is not key/AAD bound: %#v", body)
		}
		value, err := base64.StdEncoding.DecodeString(body["value_base64"])
		if err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/v1/encrypt" {
			value = append([]byte("wrapped:"), value...)
		} else if r.URL.Path == "/v1/decrypt" {
			value = bytes.TrimPrefix(value, []byte("wrapped:"))
		} else {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value_base64": base64.StdEncoding.EncodeToString(value)})
	}))
	defer server.Close()

	provider, err := NewSecretProvider(SecretProviderConfig{Environment: "development", Mode: "remote", KeyID: "kms-key-v1", RemoteURL: server.URL, RemoteToken: secret})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := provider.Encrypt([]byte("provider-secret"), []byte("credential-id"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := provider.Decrypt(ciphertext, []byte("credential-id"))
	if err != nil || string(plaintext) != "provider-secret" {
		t.Fatalf("remote round trip failed: %q, %v", plaintext, err)
	}
}

func TestProductionRemoteProviderRequiresHTTPS(t *testing.T) {
	if _, err := NewSecretProvider(SecretProviderConfig{Environment: "production-new", Mode: "remote", KeyID: "kms-key-v1", RemoteURL: "http://kms.internal", RemoteToken: "remote-provider-token-that-is-long-enough"}); err == nil {
		t.Fatal("production accepted plaintext remote secret provider")
	}
}

func TestRemoteEnvelopeProviderDoesNotFollowRedirects(t *testing.T) {
	received := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	provider, err := NewRemoteEnvelopeProvider("development", "kms-key-v1", redirect.URL, "remote-provider-token-that-is-long-enough")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Encrypt([]byte("secret"), []byte("credential-id")); err == nil {
		t.Fatal("redirect response was accepted")
	}
	if received {
		t.Fatal("authorization-bearing KMS request followed a redirect")
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
