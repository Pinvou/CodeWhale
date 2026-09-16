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

func TestApprovalBFFLocksPathAndRejectsBrowserProof(t *testing.T) {
	version, wire := int64(9), 2
	bumpAfterDetail := false
	staleDetailHeader := false
	var upstreamPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/admin/v2/approval-capability" {
			mode, proof := "PROOF_REQUIRED", true
			if wire == 1 {
				mode, proof = "SAFE_ROLLBACK", true
			}
			_, _ = fmt.Fprintf(w, `{"schema_version":1,"mode":%q,"approval_wire_version":%d,"proof_required":%t,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":%d,"request_id":"upstream"}`, mode, wire, proof, version)
			return
		}
		upstreamPath = r.URL.Path
		if strings.Contains(r.URL.Path, "/actions/decide") {
			_, _ = w.Write([]byte(`{"status":"APPROVED"}`))
			return
		}
		if wire == 1 {
			detailVersion := version
			if staleDetailHeader {
				detailVersion--
			}
			w.Header().Set("X-Pinvou-Approval-Action-Control-Version", fmt.Sprint(detailVersion))
		}
		_, _ = fmt.Fprintf(w, `{"action_control_version":%d,"allowed_actions":["APPROVE"]}`, version)
		if bumpAfterDetail {
			version++
		}
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parsed
	session := Session{ID: "new", AccessToken: "access", CSRFToken: strings.Repeat("x", 32), ApprovalV2Eligible: true,
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
	_ = store.SaveSession(context.Background(), session)
	id := "0198fbf7-c3ba-7bc4-8849-c157740963de"
	request := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		if method == http.MethodPost {
			r.Header.Set("Origin", "https://portal.example")
			r.Header.Set("X-CSRF-Token", session.CSRFToken)
			r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
			r.Header.Set("X-Pinvou-Approval-Action-Control-Version", "9")
		}
		return r
	}
	resource := "/api/admin/approvals/requests/" + id
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodGet, resource, ""))
	if response.Code != 200 || upstreamPath != "/admin/v2/approval-requests/"+id || store.approvalWires[session.ID] != 2 {
		t.Fatalf("v2 detail not locked: %d %s %s", response.Code, response.Body.String(), upstreamPath)
	}
	for _, body := range []string{
		`{"expected_version":1,"action_control_version":9,"decision":"APPROVED","reason":"valid reason","proof":{}}`,
		`{"expected_version":1,"action_control_version":8,"decision":"APPROVED","reason":"valid reason"}`,
	} {
		response = httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request(http.MethodPost, resource+"/actions/decide", body))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("untrusted decision accepted: %d %s", response.Code, response.Body.String())
		}
	}
	valid := `{"expected_version":1,"action_control_version":9,"decision":"APPROVED","reason":"valid reason"}`
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodPost, resource+"/actions/decide", valid))
	if response.Code != http.StatusNoContent || upstreamPath != "/admin/v2/approval-requests/"+id+"/actions/decide" {
		t.Fatalf("v2 decision not routed: %d %s %s", response.Code, response.Body.String(), upstreamPath)
	}
	wire = 1
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodGet, resource, ""))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_PROTOCOL_UNAVAILABLE") {
		t.Fatalf("v2 session downgraded: %d %s", response.Code, response.Body.String())
	}
	old := session
	old.ID, old.ApprovalV2Eligible = "old", false
	_ = store.SaveSession(context.Background(), old)
	legacy := request(http.MethodGet, resource, "")
	legacy.Header.Set("Cookie", service.sessionCookieName()+"="+old.ID)
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, legacy)
	if response.Code != 200 || response.Header().Get("X-Pinvou-Approval-Action-Control-Version") != "9" || upstreamPath != "/admin/v1/approval-requests/"+id {
		t.Fatalf("legacy detail not preserved: %d %s %s", response.Code, response.Body.String(), upstreamPath)
	}
	version = 10
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, legacy)
	if response.Code != 200 || response.Header().Get("X-Pinvou-Approval-Action-Control-Version") != "10" {
		t.Fatalf("new control not observed: %d %s", response.Code, response.Body.String())
	}
	staleDetailHeader = true
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, legacy)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "allowed_actions") {
		t.Fatalf("stale detail header exposed actions: %d %s", response.Code, response.Body.String())
	}
	staleDetailHeader, bumpAfterDetail = false, true
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, legacy)
	if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "allowed_actions") {
		t.Fatalf("late control change exposed actions: %d %s", response.Code, response.Body.String())
	}
}
