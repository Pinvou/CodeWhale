package token

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTokenTransitInvalidErrorsPreserveSentinel(t *testing.T) {
	if _, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "../invalid", RemoteURL: "http://localhost", RemoteToken: "test-only-credential"}); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal("invalid configuration no longer preserves Token sentinel")
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	p, err := NewTransitProvider(SecretProviderConfig{Environment: "test", KeyID: "active", RemoteURL: server.URL, RemoteToken: "test-only-credential"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Encrypt(nil, []byte("aad")); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal("encrypt invalid input lost Token sentinel")
	}
	if _, err = p.DecryptWithKeyID("../invalid", []byte("vault:v1:YWJjZA=="), []byte("aad")); !errors.Is(err, ErrInvalidSecret) {
		t.Fatal("decrypt invalid key lost Token sentinel")
	}
	if calls != 0 {
		t.Fatal("invalid Token input reached the KMS")
	}
}
