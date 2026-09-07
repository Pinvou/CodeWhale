package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTransitProviderNativeProtocolAndRotation(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	first, second := "test-transit-credential-one", "test-transit-credential-two"
	if err := os.WriteFile(file, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Vault-Token"))
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Error("incorrect native authentication")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if body["associated_data"] != base64.StdEncoding.EncodeToString([]byte("credential-a")) {
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/v1/transit/encrypt/active-key":
			if body["plaintext"] != base64.StdEncoding.EncodeToString([]byte("secret-value")) {
				t.Error("plaintext encoding")
			}
			_, _ = w.Write([]byte(`{"data":{"ciphertext":"vault:v2:YWJjZA=="}}`))
		case "/v1/transit/decrypt/previous-key":
			if body["ciphertext"] != "vault:v2:YWJjZA==" {
				t.Error("ciphertext must remain opaque")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString([]byte("secret-value"))}})
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	provider, err := NewSecretProvider(SecretProviderConfig{Environment: "production", Mode: "vault-transit", KeyID: "active-key", RemoteURL: server.URL, RemoteTokenFile: file})
	if err != nil {
		t.Fatal(err)
	}
	p := provider.(*TransitProvider)
	// Only this TLS fixture's private CA is trusted; production uses the system CA pool.
	p.client.Transport = server.Client().Transport
	ciphertext, err := p.Encrypt([]byte("secret-value"), []byte("credential-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	plain, err := p.DecryptWithKeyID("previous-key", ciphertext, []byte("credential-a"))
	if err != nil || string(plain) != "secret-value" {
		t.Fatal("native round trip failed", err)
	}
	if len(seen) != 2 || seen[0] != first || seen[1] != second {
		t.Fatal("rotated credential was not read")
	}
	if _, err := p.DecryptWithKeyID("previous-key", ciphertext, []byte("wrong-aad")); err == nil {
		t.Fatal("wrong AAD accepted")
	}
	if _, err := p.DecryptWithKeyID("forbidden-key", ciphertext, []byte("credential-a")); err == nil {
		t.Fatal("forbidden key accepted")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	count := len(seen)
	if _, err := p.Encrypt([]byte("secret"), []byte("aad")); err == nil || len(seen) != count {
		t.Fatal("missing sink reused cached token")
	}
}

func TestTransitProviderRejectsUnsafeConfiguration(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("test-credential-only"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"http://kms.example", "https://u:p@kms.example", "https://kms.example?x=1", "https://kms.example?", "https://kms.example/#x", "https://kms.example/v1", "https://kms.example/%2f"} {
		if _, err := NewTransitProvider(SecretProviderConfig{Environment: "production", KeyID: "key", RemoteURL: raw, RemoteTokenFile: file}); err == nil {
			t.Fatalf("unsafe configuration accepted: %s", raw)
		}
	}
	for _, key := range []string{"", ".", "..", "../keys", "key/rotate", "key?x", "key%2frotate"} {
		if _, err := NewTransitProvider(SecretProviderConfig{Environment: "production", KeyID: key, RemoteURL: "https://kms.example", RemoteTokenFile: file}); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	for _, cfg := range []SecretProviderConfig{
		{Environment: "production", KeyID: "key", RemoteURL: "https://kms.example", RemoteToken: "test-credential-only"},
		{Environment: "production-new", KeyID: "key", RemoteURL: "https://kms.example", RemoteTokenFile: "relative-token"},
		{Environment: "production", KeyID: "key", RemoteURL: "https://kms.example", RemoteTokenFile: file, RemoteToken: "ambiguous-credential"},
	} {
		if _, err := NewTransitProvider(cfg); err == nil {
			t.Fatal("unsafe credential source accepted")
		}
	}
}

func TestTransitProviderRejectsRemoteFailuresWithoutLeaks(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"denied", `secret-canary`, 403}, {"sealed", `secret-canary`, 503},
		{"malformed", `secret-canary`, 200}, {"empty", `{"data":{}}`, 200},
		{"trailing", `{"data":{"ciphertext":"vault:v1:YWJjZA=="}} {}`, 200},
		{"invalid-envelope", `{"data":{"ciphertext":"secret-canary"}}`, 200},
		{"large", strings.Repeat("x", 4*transitMaximumValue+1), 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "key", RemoteURL: server.URL, RemoteToken: "test-credential-only"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.Encrypt([]byte("secret-canary"), []byte("aad")); err == nil || strings.Contains(err.Error(), "secret-canary") {
				t.Fatal("failure accepted or secret leaked")
			}
		})
	}
}

func TestTransitProviderRejectsRedirectAndUntrustedTLS(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	p, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "key", RemoteURL: redirect.URL, RemoteToken: "test-credential-only"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Encrypt([]byte("secret"), []byte("aad")); err == nil || called {
		t.Fatal("authenticated redirect followed")
	}
	untrusted := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer untrusted.Close()
	p.baseURL = untrusted.URL
	if _, err = p.Encrypt([]byte("secret"), []byte("aad")); err == nil || called {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestTransitProviderRejectsInvalidInputsBeforeNetwork(t *testing.T) {
	called := false
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer s.Close()
	p, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "key", RemoteURL: s.URL, RemoteToken: "test-credential-only"})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range [][]byte{nil, bytes.Repeat([]byte{1}, transitMaximumValue+1)} {
		if _, err = p.Encrypt(v, []byte("aad")); err == nil {
			t.Fatal("invalid plaintext accepted")
		}
	}
	if _, err = p.Encrypt([]byte("secret"), nil); err == nil {
		t.Fatal("empty AAD accepted")
	}
	if _, err = p.DecryptWithKeyID("../keys", []byte("vault:v1:YWJjZA=="), []byte("aad")); err == nil {
		t.Fatal("key path traversal accepted")
	}
	if _, err = p.Decrypt([]byte("unknown-format"), []byte("aad")); err == nil {
		t.Fatal("invalid envelope accepted")
	}
	if called {
		t.Fatal("invalid inputs reached KMS")
	}
}
