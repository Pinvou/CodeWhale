package authorization

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestRetainedAuthorizationReplyMustBeExplicitCompleteAndBounded(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"explicit-denial", `{"allowed":false,"future_metadata":"kept"}`, true},
		{"missing-decision", `{}`, false},
		{"null-decision", `{"allowed":null}`, false},
		{"wrong-type", `{"allowed":"false"}`, false},
		{"second-json", `{"allowed":true} {"allowed":false}`, false},
		{"trailing-data", `{"allowed":true} invalid`, false},
		{"over-limit", `{"allowed":true,"metadata":"` + strings.Repeat("x", 64<<10) + `"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/authorization/check" {
					t.Error("authorization did not use its actual service endpoint")
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "test-service-token")
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = client.Allowed(context.Background(), identity.Principal{Issuer: "test", Subject: "user"}, "personal.billing.read")
			if (err == nil) != test.valid {
				t.Fatalf("response valid = %v, want %v (error %v)", err == nil, test.valid, err)
			}
		})
	}
}

func TestRetainedAuthorizationClientRejectsUnsafeEndpointsAndRedirects(t *testing.T) {
	for _, endpoint := range []string{"file://host/path", "https://user:secret@example.test", "https://example.test/?mode=test", "https://example.test/?", "https://example.test/#fragment"} {
		if _, err := NewClient(endpoint, "test-service-token"); err == nil {
			t.Fatalf("accepted unsafe authorization endpoint %q", endpoint)
		}
	}
	var forwarded int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		_, _ = w.Write([]byte(`{"allowed":true}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := NewClient(source.URL, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Allowed(context.Background(), identity.Principal{}, "personal.billing.read"); err == nil {
		t.Fatal("a redirected authorization reply was trusted")
	}
	if forwarded != 0 {
		t.Fatal("a service credential was forwarded to the redirect destination")
	}
}
