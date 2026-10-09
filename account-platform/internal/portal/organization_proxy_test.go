package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOrganizationProxyFiniteDeliveredRoutes(t *testing.T) {
	org, target := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		area, method, path string
		want               bool
	}{
		{"account", "GET", "/v1/me/organizations", true},
		{"account", "POST", "/v1/sessions/current/active-context", true},
		{"account", "PATCH", "/v1/organizations/" + org + "/units/" + target + "/parent", true},
		{"account", "DELETE", "/v1/organizations/" + org + "/role-bindings/" + target, true},
		{"token", "GET", "/v1/organizations/" + org + "/recharge/products", true},
		{"token", "GET", "/v1/organizations/" + org + "/payment-capabilities", true},
		{"token", "POST", "/v1/organizations/" + org + "/quota-grants:batch", true},
		{"admin", "PATCH", "/admin/v1/organizations/" + org + "/status", true},
		{"admin", "DELETE", "/admin/v1/organizations/" + org, true},
		{"token", "POST", "/v1/organizations/" + org + "/grants/" + target + "/account-closed-reclaim", false},
		{"account", "POST", "/v1/organizations/" + org + "/gifts", false},
		{"admin", "DELETE", "/admin/v1/users/" + target, false},
		{"account", "DELETE", "/v1/me/closure-requests", false},
		{"token", "POST", "/v1/organizations/00000000-0000-0000-0000-000000000000/quota-grants:batch", false},
		{"account", "PUT", "/v1/organizations/" + org + "/units/" + target + "/parent", false},
	} {
		t.Run(tc.area+"/"+tc.method+tc.path, func(t *testing.T) {
			var got bool
			switch tc.area {
			case "account":
				got = organizationAccountPathAllowed(tc.method, tc.path)
			case "token":
				got = organizationTokenPathAllowed(tc.method, tc.path)
			default:
				got = organizationAdminPathAllowed(tc.method, tc.path)
			}
			if got != tc.want {
				t.Fatalf("allowed=%v want=%v", got, tc.want)
			}
		})
	}
}

// Real Portal session/CSRF middleware and proxy use a controlled Token HTTP
// authority. No Redis, provider payment or organization invocation is claimed.
func TestOrganizationQuotaProxyPreservesExactMoneyAndSession(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	org := uuid.NewString()
	path := "/v1/organizations/" + org + "/quota-grants:batch"
	body := `{"items":[{"amount_microcredits":9007199254740993}]}`
	receipt := `{"items":[{"index":0,"status":"FAILED","code":"INSUFFICIENT_COMPONENT_CAPACITY"}]}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != body || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != "owned-organization-quota-key" || r.Header.Get("X-CSRF-Token") != "" {
			t.Error("organization proxy lost exact facts or session authority")
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, receipt)
	}))
	defer upstream.Close()
	service.config.TokenGatewayURL = parseTestURL(t, upstream.URL)
	request := financeBrowserRequest(service, session, http.MethodPost, "/api/token"+path, body)
	request.Header.Set("Idempotency-Key", "owned-organization-quota-key")
	request.Header.Set("Authorization", "Bearer untrusted-browser")
	out := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(out, request)
	if out.Code != 207 || out.Body.String() != receipt || calls.Load() != 1 {
		t.Fatal("organization batch did not preserve current upstream result")
	}
	request = financeBrowserRequest(service, session, http.MethodPost, "/api/token"+path, body)
	request.Header.Set("X-CSRF-Token", "wrong")
	out = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(out, request)
	if out.Code != 403 || calls.Load() != 1 {
		t.Fatal("invalid CSRF reached organization authority")
	}
	for _, suffix := range []string{"/gifts", "/grants/" + uuid.NewString() + "/account-closed-reclaim"} {
		request = financeBrowserRequest(service, session, http.MethodPost, "/api/token/v1/organizations/"+org+suffix, `{}`)
		out = httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(out, request)
		if out.Code != 404 || calls.Load() != 1 {
			t.Fatal("future organization path reached authority")
		}
	}
}
