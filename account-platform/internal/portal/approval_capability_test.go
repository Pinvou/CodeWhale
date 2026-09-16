package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPortalApprovalCapabilityLocksTrustedWireAndNeverDowngrades(t *testing.T) {
	wire := 2
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/v2/approval-capability" || r.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("unexpected Account capability request: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		mode, proof := "PROOF_REQUIRED", true
		if wire == 1 {
			mode, proof = "SAFE_ROLLBACK", true
		}
		_ = json.NewEncoder(w).Encode(trustedApprovalCapability{SchemaVersion: 1, Mode: mode, ApprovalWireVersion: wire,
			ProofRequired: proof, Epoch: 7, AuthorizationRelayState: "RUNNING", AuthorizationRelayEpoch: 2,
			Version: 11, RequestID: "account-request"})
	}))
	defer upstream.Close()
	parsed, _ := url.Parse(upstream.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parsed
	session := Session{ID: "new-session", AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour),
		RefreshExpiresAt: time.Now().Add(2 * time.Hour), ApprovalV2Eligible: true}
	_ = store.SaveSession(context.Background(), session)
	request := func(id string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/admin/approvals/capability", nil)
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: id})
		return r
	}
	response := httptest.NewRecorder()
	service.adminApprovalCapability(response, request(session.ID))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		!strings.Contains(response.Body.String(), `"session_approval_wire_version":2`) {
		t.Fatalf("v2 capability not locked: status=%d body=%s", response.Code, response.Body.String())
	}
	wire = 1
	response = httptest.NewRecorder()
	service.adminApprovalCapability(response, request(session.ID))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_PROTOCOL_UNAVAILABLE") || store.approvalWires[session.ID] != 2 {
		t.Fatalf("v2 session downgraded after rollback: status=%d body=%s", response.Code, response.Body.String())
	}
	wire = 2
	old := session
	old.ID = "old-session"
	old.ApprovalV2Eligible = false
	_ = store.SaveSession(context.Background(), old)
	response = httptest.NewRecorder()
	service.adminApprovalCapability(response, request(old.ID))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"session_approval_wire_version":1`) {
		t.Fatalf("pre-upgrade session did not remain v1: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTrustedApprovalCapabilityRejectsDuplicateMissingAndUnknownFacts(t *testing.T) {
	valid := `{"schema_version":1,"mode":"LEGACY","approval_wire_version":1,"proof_required":false,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"request"}`
	for _, raw := range []string{
		strings.Replace(valid, `"version":1`, `"version":1,"version":2`, 1),
		strings.Replace(valid, `,"request_id":"request"`, "", 1),
		strings.Replace(valid, `"mode":"LEGACY"`, `"mode":"LEGACY","browser_mode":"PROOF_REQUIRED"`, 1),
		strings.Replace(valid, `"approval_wire_version":1`, `"approval_wire_version":2`, 1),
	} {
		if _, err := decodeTrustedApprovalCapability([]byte(raw)); err == nil {
			t.Fatalf("unsafe capability accepted: %s", raw)
		}
	}
	if _, err := decodeTrustedApprovalCapability([]byte(valid)); err != nil {
		t.Fatalf("valid closed capability rejected: %v", err)
	}
}

func TestPortalApprovalCapabilityFailsClosedOnUntrustedAccountResponse(t *testing.T) {
	valid := `{"schema_version":1,"mode":"LEGACY","approval_wire_version":1,"proof_required":false,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"request"}`
	for _, tc := range []struct {
		name, body, cache string
		status            int
	}{
		{"cacheable", valid, "public", http.StatusOK},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":1,"version":2`, 1), "no-store", http.StatusOK},
		{"redirect", valid, "no-store", http.StatusFound},
		{"oversized", valid + strings.Repeat(" ", 4096), "no-store", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", tc.cache)
				w.Header().Set("Location", "/other")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			parsed, _ := url.Parse(upstream.URL)
			store := newMemoryStore()
			service := testService(t, store)
			service.config.AccountURL = parsed
			session := Session{ID: "new", AccessToken: "access", ApprovalV2Eligible: true,
				AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
			_ = store.SaveSession(context.Background(), session)
			request := httptest.NewRequest(http.MethodGet, "/api/admin/approvals/capability", nil)
			request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
			response := httptest.NewRecorder()
			service.adminApprovalCapability(response, request)
			if response.Code != http.StatusServiceUnavailable || len(store.approvalWires) != 0 {
				t.Fatalf("untrusted response selected protocol: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
