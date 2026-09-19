package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const approvalPrecheckProjection = `{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,` +
	`"target_resource_type":"RECEIVABLE","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de",` +
	`"normalized_command":{"receivable_id":"0198fbf7-c3ba-7bc4-8849-c157740963de"},` +
	`"critical_resource_versions":{"receivable_version":3},"financial_snapshot":{"outstanding_microcredits":500}}`

func approvalPrecheckFixture(t *testing.T, upstream http.HandlerFunc) (*Service, Session, func(method, path, body string, mutate func(*http.Request)) *http.Request) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	parsed, _ := url.Parse(server.URL)
	store := newMemoryStore()
	service := testService(t, store)
	service.config.AccountURL = parsed
	session := Session{ID: "precheck", AccessToken: "access", CSRFToken: strings.Repeat("x", 32),
		AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(2 * time.Hour)}
	_ = store.SaveSession(context.Background(), session)
	request := func(method, path, body string, mutate func(*http.Request)) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
		if method == http.MethodPost {
			r.Header.Set("Origin", "https://portal.example")
			r.Header.Set("X-CSRF-Token", session.CSRFToken)
			r.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
		}
		if mutate != nil {
			mutate(r)
		}
		return r
	}
	return service, session, request
}

func TestApprovalPrecheckBFFForwardsClosedInputWithUserIdentity(t *testing.T) {
	var upstreamPath, authorization, forwarded string
	service, session, request := approvalPrecheckFixture(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		authorization = r.Header.Get("Authorization")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		forwarded = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(approvalPrecheckProjection))
	})
	body := `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de",` +
		`"write_off_microcredits":500,"expected_version":3}`
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request(http.MethodPost, "/api/admin/approvals/prechecks", body, nil))
	if response.Code != http.StatusOK || service.config.AccountURL == nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if upstreamPath != "/admin/v2/approval-command-prechecks" {
		t.Fatalf("upstream path=%s", upstreamPath)
	}
	if authorization != "Bearer "+session.AccessToken {
		t.Fatalf("the user access token must authorize the account call, got %q", authorization)
	}
	if forwarded != body {
		t.Fatalf("the closed input must be forwarded verbatim, got %s", forwarded)
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers=%v", response.Header())
	}
	if response.Body.String() != approvalPrecheckProjection {
		t.Fatalf("projection must pass through verbatim, got %s", response.Body.String())
	}
}

func TestApprovalPrecheckBFFRejectsUnauthenticatedAndUnclosedRequests(t *testing.T) {
	calls := 0
	service, _, request := approvalPrecheckFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(approvalPrecheckProjection))
	})
	handler := service.Handler(nil)
	validBody := `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de"}`

	// The answer leaks owner facts, so the CSRF token is mandatory.
	missingCSRF := httptest.NewRequest(http.MethodPost, "/api/admin/approvals/prechecks", strings.NewReader(validBody))
	missingCSRF.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: "precheck"})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, missingCSRF)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	// No session at all is a login redirect surface, never an anonymous read.
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodPost, "/api/admin/approvals/prechecks", strings.NewReader(validBody)))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", anonymous.Code)
	}
	for name, body := range map[string]string{
		"unknown key":        `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de","holder":"x"}`,
		"duplicate key":      `{"command_type":"RECEIVABLE_WRITE_OFF","command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de"}`,
		"missing target":     `{"command_type":"RECEIVABLE_WRITE_OFF"}`,
		"padded target":      `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":" 0198fbf7-c3ba-7bc4-8849-c157740963de"}`,
		"not an object":      `[]`,
		"trailing json":      validBody + `{}`,
		"empty command type": `{"command_type":"","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodPost, "/api/admin/approvals/prechecks", body, nil))
			if recorder.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
			}
		})
	}
	for name, mutate := range map[string]func(*http.Request){
		"query string": func(r *http.Request) { r.URL.RawQuery = "command_type=LEDGER_REPAIR" },
		"browser proof": func(r *http.Request) {
			r.Header.Set("X-Pinvou-Approval-Proof-Verdict", "ALLOW")
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodPost, "/api/admin/approvals/prechecks", validBody, mutate))
			if recorder.Code != http.StatusBadRequest || calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls, recorder.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatalf("rejected requests reached the account service: calls=%d", calls)
	}
}

func TestApprovalPrecheckBFFNeverPresentsAnUnclosedProjection(t *testing.T) {
	for name, test := range map[string]struct {
		status int
		body   string
		expect int
	}{
		"owner refusal":   {status: http.StatusUnprocessableEntity, body: `{"code":"APPROVAL_PRECHECK_UNAVAILABLE"}`, expect: http.StatusUnprocessableEntity},
		"owner timeout":   {status: http.StatusGatewayTimeout, body: `{"code":"APPROVAL_PRECHECK_TIMEOUT"}`, expect: http.StatusGatewayTimeout},
		"owner forbidden": {status: http.StatusForbidden, body: `{"code":"PERMISSION_DENIED"}`, expect: http.StatusForbidden},
		"missing field":   {status: http.StatusOK, body: `{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de","normalized_command":{},"critical_resource_versions":{}}`, expect: http.StatusServiceUnavailable},
		"extra field":     {status: http.StatusOK, body: strings.TrimSuffix(approvalPrecheckProjection, "}") + `,"extra":1}`, expect: http.StatusServiceUnavailable},
		"zero version":    {status: http.StatusOK, body: `{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":0,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de","normalized_command":{},"critical_resource_versions":{},"financial_snapshot":{}}`, expect: http.StatusServiceUnavailable},
		"scalar document": {status: http.StatusOK, body: `{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de","normalized_command":"x","critical_resource_versions":{},"financial_snapshot":{}}`, expect: http.StatusServiceUnavailable},
		"redirect":        {status: http.StatusTemporaryRedirect, body: ``, expect: http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			service, _, request := approvalPrecheckFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if test.status >= 300 && test.status < 400 {
					w.Header().Set("Location", "/admin/v2/approval-command-prechecks")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			body := `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"0198fbf7-c3ba-7bc4-8849-c157740963de"}`
			recorder := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(recorder, request(http.MethodPost, "/api/admin/approvals/prechecks", body, nil))
			if recorder.Code != test.expect {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Code != test.expect || (test.expect == http.StatusServiceUnavailable && !strings.Contains(recorder.Body.String(), "APPROVAL_PROTOCOL_UNAVAILABLE")) {
				t.Fatalf("unclosed answer must stay unavailable: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
