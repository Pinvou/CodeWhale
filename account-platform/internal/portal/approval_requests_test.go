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
	historical := false
	commandType := "WALLET_ADJUSTMENT"
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
		if historical {
			_, _ = w.Write([]byte(`{"detail_availability":"DETAIL_PRESENTATION_UNAVAILABLE","status":"SUBMITTED"}`))
			return
		}
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
		_, _ = fmt.Fprintf(w, `{"approval_request_id":"0198fbf7-c3ba-7bc4-8849-c157740963de","command_type":%q,"action_control_version":%d,"allowed_actions":["APPROVE"]}`, commandType, version)
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
	commandType = "TOKEN_CREDENTIAL_REVOKE"
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodGet, resource, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("token approval detail should remain readable: %d", response.Code)
	}
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodPost, resource+"/actions/decide", valid))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled Token approval action passed BFF: %d %s", response.Code, response.Body.String())
	}
	commandType = "WALLET_ADJUSTMENT"
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
	bumpAfterDetail, historical, wire = false, true, 2
	version++
	historySession := session
	historySession.ID = "historical"
	_ = store.SaveSession(context.Background(), historySession)
	historyRequest := request(http.MethodGet, resource, "")
	historyRequest.Header.Set("Cookie", service.sessionCookieName()+"="+historySession.ID)
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, historyRequest)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "allowed_actions") {
		t.Fatalf("historical minimum was not readable without actions: %d %s", response.Code, response.Body.String())
	}
}

func TestApprovalBFFDraftLifecycleUsesLockedWire(t *testing.T) {
	const id = "0198fbf7-c3ba-7bc4-8849-c157740963de"
	var paths []string
	var forwardedControl string
	var version int64 = 7
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/admin/v2/approval-capability" {
			_, _ = fmt.Fprintf(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":%d,"request_id":"upstream"}`, version)
			return
		}
		paths = append(paths, r.Method+" "+r.URL.Path)
		forwardedControl = r.Header.Get("X-Pinvou-Approval-Action-Control-Version")
		if r.Method == http.MethodPost && r.URL.Path == "/admin/v1/approval-requests" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"approval_request_id":"` + id + `","status":"DRAFT"}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/admin/v2/approval-requests" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"approval_request_id":"` + id + `","status":"DRAFT"}`))
			return
		}
		_, _ = w.Write([]byte(`{"approval_request_id":"` + id + `","status":"SUBMITTED"}`))
	}))
	defer server.Close()
	parsed, _ := url.Parse(server.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parsed
	service.config.AdminModelTokenActionsEnabled = true
	session := Session{ID: "v2-lifecycle", AccessToken: "access", CSRFToken: strings.Repeat("x", 32), ApprovalV2Eligible: true,
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
	_ = store.SaveSession(context.Background(), session)
	invoke := func(method, resource, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "/api/admin/approvals/requests"+resource, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
		r.Header.Set("Origin", "https://portal.example")
		r.Header.Set("X-CSRF-Token", session.CSRFToken)
		r.Header.Set("X-Pinvou-Approval-Action-Control-Version", "7")
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, r)
		return response
	}
	draft := `{"command_type":"WALLET_ADJUSTMENT","idempotency_key":"test-v2-draft","normalized_command":{},"target_resource_type":"MANUAL_ADJUSTMENT","target_resource_id":"target","reason":"independent repair","critical_resource_versions":{},"financial_snapshot":{}}`
	created := invoke(http.MethodPost, "", draft)
	if created.Code != http.StatusCreated || strings.TrimSpace(created.Body.String()) != `{"approval_request_id":"`+id+`"}` || forwardedControl != "7" {
		t.Fatalf("create not routed or closed: %d %s %s", created.Code, created.Body.String(), forwardedControl)
	}
	updated := invoke(http.MethodPut, "/"+id, strings.TrimSuffix(draft, "}")+`,"expected_version":1}`)
	if updated.Code != http.StatusNoContent || forwardedControl != "7" {
		t.Fatalf("update not routed: %d %s", updated.Code, updated.Body.String())
	}
	for _, action := range []string{"submit", "withdraw"} {
		response := invoke(http.MethodPost, "/"+id+"/actions/"+action, `{"expected_version":2}`)
		if response.Code != http.StatusNoContent || forwardedControl != "7" {
			t.Fatalf("%s not routed: %d %s", action, response.Code, response.Body.String())
		}
	}
	if len(paths) != 4 || paths[0] != "POST /admin/v2/approval-requests" || paths[1] != "PUT /admin/v2/approval-requests/"+id ||
		paths[2] != "POST /admin/v2/approval-requests/"+id+"/actions/submit" || paths[3] != "POST /admin/v2/approval-requests/"+id+"/actions/withdraw" {
		t.Fatalf("unexpected upstream paths: %v", paths)
	}
	legacy := session
	legacy.ID, legacy.ApprovalV2Eligible = "v1-lifecycle", false
	_ = store.SaveSession(context.Background(), legacy)
	legacyRequest := httptest.NewRequest(http.MethodPost, "/api/admin/approvals/requests", strings.NewReader(draft))
	legacyRequest.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: legacy.ID})
	legacyRequest.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: legacy.CSRFToken})
	legacyRequest.Header.Set("Origin", "https://portal.example")
	legacyRequest.Header.Set("X-CSRF-Token", legacy.CSRFToken)
	legacyRequest.Header.Set("X-Pinvou-Approval-Action-Control-Version", "7")
	legacyCreated := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(legacyCreated, legacyRequest)
	if legacyCreated.Code != http.StatusCreated || !strings.Contains(legacyCreated.Body.String(), id) || paths[len(paths)-1] != "POST /admin/v1/approval-requests" || forwardedControl != "" {
		t.Fatalf("legacy session not locked to v1: %d %s %v", legacyCreated.Code, legacyCreated.Body.String(), paths)
	}
	for _, bad := range []string{strings.TrimSuffix(draft, "}") + `,"proof":{}}`, strings.TrimSuffix(draft, "}") + `,"command_type":"TOKEN_CREDENTIAL_REVOKE"}`} {
		response := invoke(http.MethodPost, "", bad)
		if response.Code != http.StatusBadRequest || len(paths) != 5 {
			t.Fatalf("untrusted draft forwarded: %d %s", response.Code, response.Body.String())
		}
	}
	service.config.AdminModelTokenActionsEnabled = false
	response := invoke(http.MethodPost, "", strings.Replace(draft, "WALLET_ADJUSTMENT", "TOKEN_CREDENTIAL_REVOKE", 1))
	if response.Code != http.StatusNotFound || len(paths) != 5 {
		t.Fatalf("disabled Token draft forwarded: %d %s", response.Code, response.Body.String())
	}
	version = 8
	response = invoke(http.MethodPost, "", draft)
	if response.Code != http.StatusBadRequest || len(paths) != 5 {
		t.Fatalf("stale control forwarded: %d %s", response.Code, response.Body.String())
	}
}
