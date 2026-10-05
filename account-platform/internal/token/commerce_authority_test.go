package token

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCommerceCustomerAuthorityConsumesCompleteBoundedFacts(t *testing.T) {
	for _, item := range []struct {
		name, body string
		status     int
		want       string
		forbidden  bool
	}{
		{"ready", `{"status":"READY"}`, 200, "READY", false},
		{"community", `{"status":"NOT_CONFIGURED"}`, 200, "NOT_CONFIGURED", false},
		{"pending", `{"status":"PENDING"}`, 200, "PENDING", false},
		{"second-json", `{"status":"READY"}{}`, 200, "", false},
		{"oversized", `{"status":"READY"}` + strings.Repeat(" ", 64<<10), 200, "", false},
		{"missing", `{}`, 200, "", false},
		{"unknown", `{"status":"UNKNOWN"}`, 200, "", false},
		{"forbidden", `{}`, 403, "", true},
		{"unauthorized", `{}`, 401, "", true},
		{"unavailable", `{}`, 500, "", false},
	} {
		t.Run(item.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me/commerce-customer-mapping" || r.Header.Get("Authorization") != "Bearer fixture" {
					t.Error("authority request mismatch")
				}
				w.WriteHeader(item.status)
				_, _ = w.Write([]byte(item.body))
			}))
			defer server.Close()
			resolver, err := NewHTTPContextResolver(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolver.ResolveCommerceCustomer(context.Background(), "Bearer fixture")
			if item.want != "" {
				if err != nil || got != item.want {
					t.Fatalf("valid status not preserved: %q %v", got, err)
				}
				return
			}
			if err == nil || got != "" || errors.Is(err, ErrForbidden) != item.forbidden {
				t.Fatalf("invalid authority accepted or failure misclassified: %q %v", got, err)
			}
		})
	}
}

func TestAccountAuthorityDoesNotFollowRedirectOrAcceptUnsafeBaseURL(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"status":"READY"}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	resolver, err := NewHTTPContextResolver(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveCommerceCustomer(context.Background(), "Bearer fixture"); err == nil || calls.Load() != 0 {
		t.Fatal("redirect followed as authority")
	}
	for _, raw := range []string{"ftp://authority.invalid", "http://caller@authority.invalid", "https://authority.invalid?route=other", "https://authority.invalid#other"} {
		if _, err := NewHTTPContextResolver(raw); err == nil {
			t.Fatal("unsafe authority configuration accepted")
		}
	}
}
