package billing

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The development token deliberately has no Account service identity. Even a
// valid legacy token cannot bypass the owner handler's Account-only constraint.
func TestF09BillingPrecheckInstalledAuthentication(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	handler := NewService(nil).Handler("controlled-precheck")
	for _, tc := range []struct {
		name, method, credential string
		status                   int
		code                     string
	}{
		{"missing-token", "POST", "", 401, "SERVICE_AUTH_REQUIRED"},
		{"wrong-token", "POST", "wrong", 401, "SERVICE_AUTH_REQUIRED"},
		{"non-account-legacy-identity", "POST", "controlled-precheck", 403, "SERVICE_AUTH_FORBIDDEN"},
		{"wrong-method", "GET", "controlled-precheck", 405, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/internal/v1/approval-command-prechecks", strings.NewReader(`{}`))
			if tc.credential != "" {
				request.Header.Set("Authorization", "Bearer "+tc.credential)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status || (tc.code != "" && !strings.Contains(response.Body.String(), tc.code)) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.status == 403 && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("precheck failure must not be cached")
			}
		})
	}
}
