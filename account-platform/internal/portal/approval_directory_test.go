package portal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestApprovalDirectoryUsesTrustedSessionAndClosedAccountProjection(t *testing.T) {
	version := int64(7)
	bad := false
	var upstreamPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/admin/v2/approval-capability" {
			_, _ = fmt.Fprintf(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":%d,"request_id":"upstream"}`, version)
			return
		}
		upstreamPath = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer access" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-Platform-Authorization-Version", "3")
		if bad {
			_, _ = w.Write([]byte(`{"schema_version":1,"platform_authorization_version":3,"items":[{"command_type":"WALLET_ADJUSTMENT","policy_version":1,"snapshot_schema_version":1,"risk_class":"FINANCIAL_FACT_CHANGE","visible":true,"requestable":true,"secret":"unsafe"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"schema_version":1,"platform_authorization_version":3,"items":[{"command_type":"WALLET_ADJUSTMENT","policy_version":1,"snapshot_schema_version":1,"risk_class":"FINANCIAL_FACT_CHANGE","visible":true,"requestable":true}]}`))
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parsed
	session := Session{ID: "directory-session", AccessToken: "access", ApprovalV2Eligible: true,
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
	_ = store.SaveSession(context.Background(), session)
	invoke := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, r)
		return response
	}
	response := invoke("/api/admin/approvals/directory")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "WALLET_ADJUSTMENT") ||
		response.Header().Get("X-Platform-Authorization-Version") != "3" || upstreamPath != "/admin/v2/approval-directory" {
		t.Fatalf("directory not closed or routed: %d %s %s", response.Code, response.Body.String(), upstreamPath)
	}
	bad = true
	response = invoke("/api/admin/approvals/directory")
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("extra field leaked: %d %s", response.Code, response.Body.String())
	}
	response = invoke("/api/admin/approvals/directory?extra=1")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("directory accepted query: %d %s", response.Code, response.Body.String())
	}
}
