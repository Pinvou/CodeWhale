package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func TestAdminNewAPIStatusUsesOnlyConfiguredProviderAndStaticResult(t *testing.T) {
	for _, scenario := range []struct {
		name, body       string
		status, expected int
	}{
		{"healthy", `{"success":true,"private_detail":"fixture-provider-detail"}`, 200, 200},
		{"unhealthy", `{"success":false}`, 200, 503},
		{"missing-success", `{}`, 200, 503},
		{"malformed", `{`, 200, 503},
		{"trailing-json", `{"success":true}{}`, 200, 503},
		{"truncated-tail", `{"success":true}` + strings.Repeat(" ", 64<<10) + "invalid-json-tail", 200, 503},
		{"oversized-json", `{"success":true,"large":"` + strings.Repeat("x", 64<<10) + `"}`, 200, 503},
		{"redirect", `{"success":true}`, 302, 503},
		{"server-failure", `{"success":true}`, 500, 503},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/api/status" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("probe changed the fixed path or forwarded browser credentials")
				}
				w.Header().Set("Location", "/should-not-follow")
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer upstream.Close()
			policy, err := NewProviderURLPolicy("test", []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			s := NewService(nil, nil, nil, nil, policy, nil, nil, 100).WithAuthorizer(tokenTestAuthorizer(t))
			if _, err := s.WithNewAPIStatus(upstream.URL); err != nil {
				t.Fatal(err)
			}
			handler := s.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://fixture-authority.invalid", Subject: "fixture-platform-operator", RealmRoles: []string{"platform-admin"}}, nil
			})
			request := httptest.NewRequest("GET", "/admin/v1/system/newapi-status?target=http://untrusted.example", nil)
			request.Header.Set("Authorization", "Bearer fixture-admin")
			request.Header.Set("Cookie", "fixture-cookie=blocked")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			want := `{"success":false}`
			if scenario.expected == 200 {
				want = `{"success":true}`
			}
			if response.Code != scenario.expected || strings.TrimSpace(response.Body.String()) != want || response.Header().Get("Cache-Control") != "no-store" || calls != 1 {
				t.Fatal("probe leaked upstream data, followed redirects or changed the health result")
			}
			denied := s.Handler(func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, nil })
			response = httptest.NewRecorder()
			denied.ServeHTTP(response, request)
			if response.Code != 403 || calls != 1 {
				t.Fatal("ordinary caller probed the protected provider")
			}
		})
	}
	policy, _ := NewProviderURLPolicy("production-new", []string{"newapi"})
	s := NewService(nil, nil, nil, nil, policy, nil, nil, 100).WithAuthorizer(tokenTestAuthorizer(t))
	for _, raw := range []string{"http://newapi:8080", "https://untrusted.example", "https://newapi:8443?target=x", "https://newapi:8443#fragment"} {
		if _, err := s.WithNewAPIStatus(raw); err == nil {
			t.Fatal("status configuration bypassed the production URL boundary")
		}
	}
}
