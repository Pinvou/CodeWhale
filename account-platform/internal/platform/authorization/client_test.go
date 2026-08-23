package authorization

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestClientUsesServiceAuthenticationAndReturnsVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/authorization/check" || r.Header.Get("Authorization") != "Bearer service-secret" {
			t.Fatalf("unexpected authorization request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("X-Context-Version", "7")
		w.Header().Set("X-Authorization-Version", "11")
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, "service-secret")
	if err != nil {
		t.Fatal(err)
	}
	allowed, headers, err := client.Allowed(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, "platform.users.read")
	if err != nil || !allowed || headers.Get("X-Context-Version") != "7" || headers.Get("X-Authorization-Version") != "11" {
		t.Fatalf("allowed=%v headers=%v error=%v", allowed, headers, err)
	}
}

func TestClientRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewClient("not-a-url", "secret"); err == nil {
		t.Fatal("relative Account URL must be rejected")
	}
	if _, err := NewClient("https://account.example", ""); err == nil {
		t.Fatal("empty service token must be rejected")
	}
}
