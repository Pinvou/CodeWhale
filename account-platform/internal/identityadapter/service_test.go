package identityadapter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIdentityRoutesRejectInvalidJSONWithExplicitError(t *testing.T) {
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), &directoryFake{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(vault, "synthetic-internal-token")
	if err != nil {
		t.Fatal(err)
	}
	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/registration/password-prechecks"},
		{http.MethodPost, "/internal/v1/registration/password-references/validate"},
		{http.MethodPost, "/internal/v1/users/mobile"},
		{http.MethodPut, "/internal/v1/users/test-subject/username"},
		{http.MethodPut, "/internal/v1/users/test-subject/mobile"},
		{http.MethodPost, "/internal/v1/users/test-subject/password-resets"},
		{http.MethodPost, "/internal/v1/users/test-subject/revoke-sessions"},
	}
	for _, route := range routes {
		for name, payload := range map[string]string{
			"malformed": "{", "unknown": `{"not_a_contract_field":true}`, "trailing": `{}invalid`,
			"second value": `{}{}`, "oversize": `{"password":"` + strings.Repeat("A", (1<<20)+1) + `"}`,
		} {
			t.Run(route.path+"/"+name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, strings.NewReader(payload))
				req.Header.Set("Authorization", "Bearer synthetic-internal-token")
				res := httptest.NewRecorder()
				service.Handler().ServeHTTP(res, req)
				if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), `"code":"INVALID_REQUEST"`) {
					t.Fatalf("bad JSON reported success: status=%d", res.Code)
				}
			})
		}
	}
}
