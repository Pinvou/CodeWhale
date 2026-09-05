package portal

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestTokenAdminQueryProjectionAllowlistIsReadOnly(t *testing.T) {
	usageID := uuid.NewString()
	listPaths := []string{
		"/admin/v1/models", "/admin/v1/protocols", "/admin/v1/formulas",
		"/admin/v1/token-accounts", "/admin/v1/provisionings", "/admin/v1/usage-requests",
		"/admin/v1/reconciliation-cases", "/admin/v1/owner-candidates", "/admin/v1/resource-audit",
		"/admin/v1/usage-requests/" + usageID + "/evidence",
		"/admin/v1/provisionings/" + usageID + "/attempts",
	}
	for _, path := range listPaths {
		if !tokenAdminPathAllowed(http.MethodGet, path) {
			t.Fatalf("read path not allowed: %s", path)
		}
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v2/provisionings/"+usageID) {
		t.Fatal("versioned redacted provisioning detail must be allowlisted")
	}
	if !tokenAdminPathAllowed(http.MethodGet, "/admin/v2/token-accounts/"+usageID) {
		t.Fatal("versioned safe Token account detail must be allowlisted")
	}
	if !tokenCredentialIsolationPath(http.MethodGet, "/admin/v2/token-accounts/"+usageID) {
		t.Fatal("safe Token account detail must remain behind the credential isolation gate")
	}
	// Existing aggregate roots retain their previously approved write methods while
	// projection-only routes introduced by APT-121 cannot be used to mutate data.
	for _, path := range []string{
		"/admin/v1/formulas", "/admin/v1/reconciliation-cases", "/admin/v1/owner-candidates",
		"/admin/v1/resource-audit", "/admin/v1/usage-requests/" + usageID + "/evidence",
		"/admin/v1/provisionings/" + usageID + "/attempts",
	} {
		if tokenAdminPathAllowed(http.MethodPost, path) || tokenAdminPathAllowed(http.MethodDelete, path) {
			t.Fatalf("query projection must remain read-only: %s", path)
		}
	}
	for _, path := range []string{"/admin/v1/owner-candidates/" + uuid.NewString(), "/admin/v1/resource-audit/export", "/admin/v1/usage-requests/not-a-uuid/evidence", "/admin/v1/provisionings/not-a-uuid/attempts", "/admin/v1/provisionings/" + usageID + "/attempts/extra", "/admin/v2/provisionings/not-a-uuid", "/admin/v2/provisionings/" + usageID + "/attempts", "/admin/v2/token-accounts/not-a-uuid", "/admin/v2/token-accounts/" + usageID + "/extra"} {
		if tokenAdminPathAllowed(http.MethodGet, path) {
			t.Fatalf("unexpected Token admin path allowed: %s", path)
		}
	}
}

func TestTokenAdminStableRouteAllowlistIsExact(t *testing.T) {
	accountID := uuid.NewString()
	planID := uuid.NewString()
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/admin/v2/token-accounts/" + accountID + "/routes"},
		{http.MethodDelete, "/admin/v2/token-accounts/" + accountID + "/routes"},
		{http.MethodGet, "/admin/v2/token-accounts/" + accountID + "/impact/route"},
		{http.MethodGet, "/admin/v1/token-accounts/" + accountID + "/impact/status"},
		{http.MethodGet, "/admin/v1/token-accounts/" + accountID + "/impact/credential"},
		{http.MethodPost, "/admin/v1/commands"},
		{http.MethodGet, "/admin/v1/commands/" + uuid.NewString()},
		{http.MethodGet, "/admin/v1/models/" + accountID + "/impact/lifecycle"},
		{http.MethodGet, "/admin/v1/models/" + accountID + "/lifecycle-plans"},
		{http.MethodPost, "/admin/v1/models/" + accountID + "/lifecycle-plans"},
		{http.MethodGet, "/admin/v1/models/" + accountID + "/lifecycle-plans/" + planID},
		{http.MethodPost, "/admin/v1/models/" + accountID + "/lifecycle-plans/" + planID + "/cancel"},
		{http.MethodGet, "/admin/v1/model-catalog/impact/rollback"},
	} {
		if !tokenAdminPathAllowed(item.method, item.path) {
			t.Fatalf("stable route path not allowed: %s %s", item.method, item.path)
		}
	}
	for _, item := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/v2/token-accounts/" + accountID + "/routes"},
		{http.MethodGet, "/admin/v2/token-accounts/" + accountID + "/routes"},
		{http.MethodPut, "/admin/v2/token-accounts/not-a-uuid/routes"},
		{http.MethodPut, "/admin/v2/token-accounts/" + accountID + "/routes/extra"},
		{http.MethodPost, "/admin/v2/token-accounts/" + accountID + "/impact/route"},
		{http.MethodGet, "/admin/v2/token-accounts/" + accountID + "/impact/credential"},
		{http.MethodGet, "/admin/v1/commands/not-a-uuid"},
		{http.MethodDelete, "/admin/v1/commands/" + uuid.NewString()},
		{http.MethodGet, "/admin/v1/token-accounts/" + accountID + "/impact/status/extra"},
		{http.MethodPost, "/admin/v1/models/not-a-uuid/lifecycle-plans"},
		{http.MethodPost, "/admin/v1/models/" + accountID + "/lifecycle-plans/not-a-uuid/cancel"},
		{http.MethodDelete, "/admin/v1/models/" + accountID + "/lifecycle-plans/" + planID},
	} {
		if tokenAdminPathAllowed(item.method, item.path) {
			t.Fatalf("unexpected stable route path allowed: %s %s", item.method, item.path)
		}
	}
}

func TestPortalClientSourceRejectsForgedPublicForwardingHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://portal.example/api/admin/token/admin/v1/models", nil)
	request.RemoteAddr = "203.0.113.19:443"
	request.Header.Set("X-Real-IP", "198.51.100.8")
	request.Header.Set("X-Forwarded-For", "198.51.100.9")
	request.Header.Set("X-Pinvou-Client-Source", "198.51.100.10")
	if got := portalClientSource(request); got != "203.0.113.19" {
		t.Fatalf("forged public forwarding header trusted: %q", got)
	}
	request.RemoteAddr = "10.0.0.8:443"
	if got := portalClientSource(request); got != "198.51.100.8" {
		t.Fatalf("private ingress source not accepted: %q", got)
	}
}

func TestPortalSignsOwnedTokenAdminSourceHeader(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	service := &Service{config: Config{TokenAdminProxyHMACKey: key}}
	request := httptest.NewRequest(http.MethodGet, "http://token.internal/admin/v1/models?limit=20", nil)
	service.applyClientCompatibilityHeaders(request, "203.0.113.19")
	if request.Header.Get("X-Pinvou-Client-Source") != "203.0.113.19" || request.Header.Get("X-Pinvou-Client-Source-Signature") == "" {
		t.Fatalf("Portal did not emit the signed source assertion: %v", request.Header)
	}
}
