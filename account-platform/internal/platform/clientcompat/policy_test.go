package clientcompat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testPolicy(t *testing.T) Policy {
	t.Helper()
	policy, err := NewPolicy("account-platform-v1", "https://updates.example.test/latest", []Release{
		{Version: "3.2.0", ReleasedAt: "2026-08-01T00:00:00Z"},
		{Version: "3.1.0", ReleasedAt: "2026-05-01T00:00:00Z", DeprecationNoticeAt: "2026-08-31T00:00:00Z", SunsetAt: "2026-10-30T00:00:00Z"},
		{Version: "3.0.0", ReleasedAt: "2026-02-01T00:00:00Z", DeprecationNoticeAt: "2026-07-03T00:00:00Z", SunsetAt: "2026-09-01T00:00:00Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy.WithNow(func() time.Time { return time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC) })
}

func TestPolicyKeepsCurrentAndPreviousTwoReleases(t *testing.T) {
	manifest := testPolicy(t).Manifest()
	if manifest.CurrentClientVersion != "3.2.0" || manifest.MinimumSupportedClientVersion != "3.0.0" || len(manifest.Releases) != 3 {
		t.Fatalf("unexpected manifest %#v", manifest)
	}
}

func TestPolicyRejectsShortSupportAndNoticeWindows(t *testing.T) {
	_, err := NewPolicy("v1", "https://updates.example.test", []Release{{Version: "3.2.0", ReleasedAt: "2026-08-01T00:00:00Z"}, {Version: "3.1.0", ReleasedAt: "2026-07-01T00:00:00Z", DeprecationNoticeAt: "2026-08-01T00:00:00Z", SunsetAt: "2026-08-20T00:00:00Z"}, {Version: "3.0.0", ReleasedAt: "2026-06-01T00:00:00Z", DeprecationNoticeAt: "2026-07-01T00:00:00Z", SunsetAt: "2026-09-01T00:00:00Z"}})
	if err == nil {
		t.Fatal("short compatibility windows were accepted")
	}
}

func TestExpiredClientWritesReceiveStableUpgradeResponse(t *testing.T) {
	policy := testPolicy(t).WithNow(func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) })
	called := false
	handler := policy.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(ClientVersionHeader, "3.0.0")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body apiError
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusUpgradeRequired || body.Code != "CLIENT_UPGRADE_REQUIRED" || called || response.Header().Get("Link") == "" {
		t.Fatalf("unexpected upgrade response code=%d body=%s called=%v", response.Code, response.Body.String(), called)
	}
}

func TestAuthenticatedRemoteWriteCannotOmitClientVersion(t *testing.T) {
	handler := testPolicy(t).Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unversioned write reached handler") }))
	request := httptest.NewRequest(http.MethodPost, "/v1/account/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer desktop-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("unversioned write returned %d", response.Code)
	}
}

func TestExactIntegrationPathCanBypassDesktopClientVersionPolicy(t *testing.T) {
	policy := testPolicy(t)
	called := false
	handler := policy.WrapExcept(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}), "/api/v1/payment-callback/token", "/api/v1/payment-callbacks")

	request := httptest.NewRequest(http.MethodPost, "/api/v1/payment-callback/token", nil)
	request.Header.Set("Authorization", "Basic bound-provider-credential")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("exact integration path was blocked: code=%d called=%v", response.Code, called)
	}

	called = false
	request = httptest.NewRequest(http.MethodPost, "/api/v1/payment-callback/token/extra", nil)
	request.Header.Set("Authorization", "Basic bound-provider-credential")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUpgradeRequired || called {
		t.Fatalf("non-exact integration path bypassed policy: code=%d called=%v", response.Code, called)
	}
}

func TestExpiredClientCanReadLocalIndependentRemoteData(t *testing.T) {
	policy := testPolicy(t).WithNow(func() time.Time { return time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC) })
	handler := policy.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set(ClientVersionHeader, "3.0.0")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("read was blocked with %d", response.Code)
	}
}

type apiError struct {
	Code string `json:"code"`
}
