package authorization

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestAuthorizationRejectsIncompleteOrUnboundedDecisions(t *testing.T) {
	for name, body := range map[string]string{
		"missing": `{}`, "null": `null`, "null allowed": `{"allowed":null}`, "string": `{"allowed":"true"}`,
		"two values": `{"allowed":true}{"allowed":false}`, "trailing": `{"allowed":true}broken`, "oversize": `{"allowed":true}` + strings.Repeat(" ", 64<<10),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client, err := NewClient(server.URL, "test-token")
			if err != nil {
				t.Fatal(err)
			}
			if allowed, _, err := client.Allowed(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, "test.permission"); err == nil || allowed {
				t.Fatalf("corrupt authority granted: allowed=%v err=%v", allowed, err)
			}
		})
	}
}

func TestAuthorizationDoesNotFollowRedirectsWithServiceCredentials(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, err := NewClient(origin.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	if allowed, _, err := client.Allowed(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, "permission"); err == nil || allowed || called {
		t.Fatalf("redirect followed=%v allowed=%v err=%v", called, allowed, err)
	}
}

func TestAuthorizationRejectsUnsafeEndpointForms(t *testing.T) {
	for _, raw := range []string{"ftp://account.example", "https://name:password@account.example", "https://account.example?unexpected=1", "https://account.example#fragment"} {
		if _, err := NewClient(raw, "test-token"); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", raw)
		}
	}
}
