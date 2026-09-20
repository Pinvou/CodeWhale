package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestV2LockedSessionCannotWriteThroughV1Wire locks the frozen session
// protocol boundary on the generic proxy: a session that already selected the
// v2 wire never completes create/update/submit/withdraw/decide through the
// legacy v1 entries, while v1 sessions and v1 reads keep working exactly as
// before. The Account v1 wire stays available for sessions that never entered
// the approval centre, which is why the gate keys off the stored selection
// instead of the path alone.
func TestV2LockedSessionCannotWriteThroughV1Wire(t *testing.T) {
	const requestID = "0198fbf7-c3ba-7bc4-8849-c157740963de"
	var upstreamCalls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls = append(upstreamCalls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/admin/v1/approval-requests":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"approval_request_id":"`+requestID+`"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"approval_request_id":"`+requestID+`","version":1}`)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer upstream.Close()

	store := newMemoryStore()
	service := testService(t, store)
	parse := func(value string) *url.URL {
		parsed, err := url.Parse(value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	service.config.AccountURL = parse(upstream.URL)

	csrf := strings.Repeat("w", 32)
	session := func(id string, eligible bool) Session {
		value := Session{ID: id, AccessToken: "access", CSRFToken: csrf, ApprovalV2Eligible: eligible,
			AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
		if err := store.SaveSession(context.Background(), value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	call := func(value Session, method, resource, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/", strings.NewReader(body))
		request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: value.ID})
		if method != http.MethodGet && method != http.MethodHead {
			request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: value.CSRFToken})
			request.Header.Set("Origin", "https://portal.example")
			request.Header.Set("X-CSRF-Token", value.CSRFToken)
		}
		response := httptest.NewRecorder()
		service.proxy(response, request, service.config.AccountURL, resource, accountAdminPathAllowed)
		return response
	}

	// A session that enters the approval centre while the gate serves v2 gets
	// the immutable v2 selection.
	v2 := session("v2-session", true)
	var locker ApprovalWireLocker = store
	if wire, err := locker.LockApprovalWireVersion(context.Background(), v2, 2); err != nil || wire != 2 {
		t.Fatalf("v2 selection wire=%d err=%v", wire, err)
	}

	writes := []struct{ method, resource string }{
		{http.MethodPost, "/admin/v1/approval-requests"},
		{http.MethodPut, "/admin/v1/approval-requests/" + requestID},
		{http.MethodPost, "/admin/v1/approval-requests/" + requestID + "/actions/submit"},
		{http.MethodPost, "/admin/v1/approval-requests/" + requestID + "/actions/withdraw"},
		{http.MethodPost, "/admin/v1/approval-requests/" + requestID + "/actions/decide"},
	}
	for _, write := range writes {
		response := call(v2, write.method, write.resource, `{"expected_version":1,"decision":"APPROVED","reason":"independent"}`)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_PROTOCOL_UNAVAILABLE") {
			t.Fatalf("v2 session %s %s status=%d body=%s", write.method, write.resource, response.Code, response.Body.String())
		}
	}
	if len(upstreamCalls) != 0 {
		t.Fatalf("v2 session reached the v1 wire upstream: %v", upstreamCalls)
	}

	// v1 reads stay available; only the write entries are closed.
	for _, read := range []string{"/admin/v1/approval-requests", "/admin/v1/approval-requests/" + requestID} {
		response := call(v2, http.MethodGet, read, "")
		if response.Code != http.StatusOK {
			t.Fatalf("v2 session GET %s status=%d body=%s", read, response.Code, response.Body.String())
		}
	}
	if len(upstreamCalls) != 2 {
		t.Fatalf("v1 reads must still reach the upstream, calls=%v", upstreamCalls)
	}

	// A session that never entered the approval centre stays on v1 and keeps
	// every v1 write available, including the explicit v1 selection.
	for _, value := range []Session{session("v1-untouched", false), session("v1-locked", true)} {
		if value.ID == "v1-locked" {
			if wire, err := locker.LockApprovalWireVersion(context.Background(), value, 1); err != nil || wire != 1 {
				t.Fatalf("v1 selection wire=%d err=%v", wire, err)
			}
		}
		before := len(upstreamCalls)
		create := call(value, http.MethodPost, "/admin/v1/approval-requests", `{"command_type":"WALLET_ADJUSTMENT"}`)
		if create.Code != http.StatusCreated {
			t.Fatalf("%s create status=%d body=%s", value.ID, create.Code, create.Body.String())
		}
		update := call(value, http.MethodPut, "/admin/v1/approval-requests/"+requestID, `{"expected_version":1}`)
		if update.Code != http.StatusNoContent {
			t.Fatalf("%s update status=%d body=%s", value.ID, update.Code, update.Body.String())
		}
		decide := call(value, http.MethodPost, "/admin/v1/approval-requests/"+requestID+"/actions/decide", `{"expected_version":1,"decision":"APPROVED","reason":"independent"}`)
		if decide.Code != http.StatusNoContent {
			t.Fatalf("%s decide status=%d body=%s", value.ID, decide.Code, decide.Body.String())
		}
		if len(upstreamCalls) != before+3 {
			t.Fatalf("%s v1 writes must reach the upstream, before=%d after=%v", value.ID, before, upstreamCalls)
		}
	}

	// A corrupted selection fails closed instead of being read as v1.
	store.approvalWires["corrupted"] = 7
	corrupted := Session{ID: "corrupted", AccessToken: "access", CSRFToken: csrf,
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
	if err := store.SaveSession(context.Background(), corrupted); err != nil {
		t.Fatal(err)
	}
	before := len(upstreamCalls)
	response := call(corrupted, http.MethodPost, "/admin/v1/approval-requests", `{"command_type":"WALLET_ADJUSTMENT"}`)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "APPROVAL_PROTOCOL_UNAVAILABLE") {
		t.Fatalf("corrupted selection status=%d body=%s", response.Code, response.Body.String())
	}
	if len(upstreamCalls) != before {
		t.Fatalf("corrupted selection reached the upstream: %v", upstreamCalls)
	}
}

// TestApprovalV1WriteResourceBoundary pins the classifier so the gate cannot
// drift onto v1 reads, v2 paths or non-approval resources.
func TestApprovalV1WriteResourceBoundary(t *testing.T) {
	const id = "0198fbf7-c3ba-7bc4-8849-c157740963de"
	allowed := []struct{ method, resource string }{
		{http.MethodPost, "/admin/v1/approval-requests"},
		{http.MethodPut, "/admin/v1/approval-requests/" + id},
		{http.MethodPost, "/admin/v1/approval-requests/" + id + "/actions/submit"},
		{http.MethodPost, "/admin/v1/approval-requests/" + id + "/actions/withdraw"},
		{http.MethodPost, "/admin/v1/approval-requests/" + id + "/actions/decide"},
	}
	for _, sample := range allowed {
		if !approvalV1WriteResource(sample.method, sample.resource) {
			t.Fatalf("%s %s must be classified as a v1 approval write", sample.method, sample.resource)
		}
	}
	blocked := []struct{ method, resource string }{
		{http.MethodGet, "/admin/v1/approval-requests"},
		{http.MethodGet, "/admin/v1/approval-requests/" + id},
		{http.MethodPost, "/admin/v2/approval-requests"},
		{http.MethodPut, "/admin/v2/approval-requests/" + id},
		{http.MethodDelete, "/admin/v1/approval-requests/" + id},
		{http.MethodPost, "/admin/v1/approval-requests/not-a-uuid/actions/decide"},
		{http.MethodPost, "/admin/v1/approval-requests/" + id + "/actions/approve"},
		{http.MethodPost, "/admin/v1/approval-requests/" + id + "/actions/decide/extra"},
		{http.MethodPost, "/admin/v1/approval-policies"},
	}
	for _, sample := range blocked {
		if approvalV1WriteResource(sample.method, sample.resource) {
			t.Fatalf("%s %s must not be classified as a v1 approval write", sample.method, sample.resource)
		}
	}
}
