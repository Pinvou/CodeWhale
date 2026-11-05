package authorization

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestDelegationCompleteResponseAndServiceBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body    string
		status        int
		denied, valid bool
	}{
		{"valid", `{"delegation":"signed-binding","expires_in":60}`, 201, false, true},
		{"empty", `{"delegation":""}`, 201, false, false},
		{"missing", `{}`, 201, false, false},
		{"trailing", `{"delegation":"signed-binding"} {}`, 201, false, false},
		{"oversize", `{"delegation":"` + strings.Repeat("x", 64<<10) + `"}`, 201, false, false},
		{"forbidden", `{}`, 403, true, false},
		{"unavailable", `{}`, 503, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/internal/v1/authorization/human-delegations" || r.Header.Get("Authorization") != "Bearer test-service-credential" {
					t.Error("delegation transport lost its binding")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "test-service-credential")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Delegate(context.Background(), identity.Principal{Issuer: "issuer", Subject: "subject"}, DelegationRequest{Audience: "billing-service", Permission: "platform.billing.wallets.read", HTTPMethod: "GET", HTTPPath: "/admin/v1/wallets", RequestSHA256: RequestSHA256(nil)})
			if tc.valid {
				if err != nil || result != "signed-binding" {
					t.Fatalf("valid response rejected: %v", err)
				}
			} else if err == nil || result != "" {
				t.Fatal("invalid reply accepted")
			}
			if errors.Is(err, ErrPermissionDenied) != tc.denied {
				t.Fatal("availability became permission denial")
			}
		})
	}
}

func TestDelegationRedirectDoesNotForwardServiceCredential(t *testing.T) {
	var received atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(201) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(307)
	}))
	defer source.Close()
	client, err := NewClient(source.URL, "test-service-credential")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.Delegate(context.Background(), identity.Principal{}, DelegationRequest{}); err == nil {
		t.Fatal("redirect accepted")
	}
	if received.Load() != 0 {
		t.Fatal("service credential followed redirect")
	}
}
