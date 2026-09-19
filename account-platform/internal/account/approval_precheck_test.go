package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func precheckWireFixture(t *testing.T, command, targetType, targetID string) []byte {
	t.Helper()
	normalized, err := json.Marshal(map[string]any{"receivable_id": targetID, "write_off_microcredits": 500})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"command_type": command, "policy_version": 1, "snapshot_schema_version": 1,
		"target_resource_type": targetType, "target_resource_id": targetID,
		"normalized_command": json.RawMessage(normalized), "critical_resource_versions": map[string]any{"receivable_version": 3},
		"financial_snapshot": map[string]any{"outstanding_microcredits": 500},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestApprovalPrecheckClientBoundsAndBinding(t *testing.T) {
	receivableID := uuid.New()
	valid := precheckWireFixture(t, "RECEIVABLE_WRITE_OFF", "RECEIVABLE", receivableID.String())
	request := ApprovalPrecheckRequest{CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: receivableID.String(),
		WriteOffMicrocredits: 500, ExpectedVersion: 3}
	var calls atomic.Int32
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/approval-command-prechecks" ||
			r.Header.Get("Authorization") != "Bearer test-workload" || r.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("untrusted precheck request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(valid)
	}))
	defer server.Close()
	source, err := NewHTTPApprovalPrecheckSource(server.URL, "test-workload")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := source.Precheck(context.Background(), request)
	if err != nil || projection.TargetResourceID != receivableID.String() || projection.TargetResourceType != "RECEIVABLE" ||
		projection.PolicyVersion != 1 || projection.SnapshotSchemaVersion != 1 || calls.Load() != 1 {
		t.Fatalf("projection=%+v calls=%d err=%v", projection, calls.Load(), err)
	}
	// Only the closed key set is forwarded; the proxy never invents fields.
	for _, key := range []string{"command_type", "target_resource_id", "write_off_microcredits", "expected_version"} {
		if !strings.Contains(string(body), key) {
			t.Fatalf("forwarded body is missing %s: %s", key, body)
		}
	}
	for _, key := range []string{"loss_review_id", "business_date", "wrong_ledger_account_id"} {
		if strings.Contains(string(body), key) {
			t.Fatalf("forwarded body carries an empty reference %s: %s", key, body)
		}
	}

	// An unknown or malformed request never reaches the owner domain.
	for name, bad := range map[string]ApprovalPrecheckRequest{
		"unknown command": {CommandType: "FUTURE_REPAIR", TargetResourceID: receivableID.String()},
		"padded id":       {CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: " " + receivableID.String()},
		"negative amount": {CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: receivableID.String(), WriteOffMicrocredits: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := source.Precheck(context.Background(), bad); !errors.Is(err, ErrApprovalPrecheckInvalid) {
				t.Fatalf("invalid request must fail closed, got %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("invalid request reached the owner: calls=%d", calls.Load())
			}
		})
	}

	// The owner's definite refusal and every unusable answer stay distinct so the
	// proxy can expose a stable, non-leaking status.
	for name, test := range map[string]struct {
		status int
		body   []byte
		expect error
	}{
		"owner refusal":   {status: http.StatusUnprocessableEntity, body: []byte(`{"code":"BILLING_APPROVAL_PRECHECK_UNAVAILABLE"}`), expect: ErrApprovalPrecheckRejected},
		"owner timeout":   {status: http.StatusGatewayTimeout, body: []byte(`{"code":"BILLING_APPROVAL_PRECHECK_TIMEOUT"}`), expect: ErrApprovalPrecheckTimeout},
		"owner failure":   {status: http.StatusServiceUnavailable, body: []byte(`{"code":"BILLING_APPROVAL_PRECHECK_UNAVAILABLE"}`), expect: ErrApprovalPrecheckUnavailable},
		"wrong target":    {status: http.StatusOK, body: precheckWireFixture(t, "RECEIVABLE_WRITE_OFF", "RECEIVABLE", uuid.New().String()), expect: ErrApprovalPrecheckInvalid},
		"wrong type":      {status: http.StatusOK, body: precheckWireFixture(t, "RECEIVABLE_WRITE_OFF", "PAYMENT_ORDER", receivableID.String()), expect: ErrApprovalPrecheckInvalid},
		"wrong command":   {status: http.StatusOK, body: precheckWireFixture(t, "LEDGER_REPAIR", "LEDGER_POSTING", receivableID.String()), expect: ErrApprovalPrecheckInvalid},
		"duplicate keys":  {status: http.StatusOK, body: []byte(`{"command_type":"RECEIVABLE_WRITE_OFF","command_type":"RECEIVABLE_WRITE_OFF"}`), expect: ErrApprovalPrecheckInvalid},
		"unknown field":   {status: http.StatusOK, body: []byte(`{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"` + receivableID.String() + `","normalized_command":{},"critical_resource_versions":{},"financial_snapshot":{},"extra":1}`), expect: ErrApprovalPrecheckInvalid},
		"empty documents": {status: http.StatusOK, body: []byte(`{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":0,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"` + receivableID.String() + `","normalized_command":{},"critical_resource_versions":{},"financial_snapshot":null}`), expect: ErrApprovalPrecheckInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_, _ = w.Write(test.body)
			}))
			defer owner.Close()
			ownerSource, err := NewHTTPApprovalPrecheckSource(owner.URL, "test-workload")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ownerSource.Precheck(context.Background(), request); !errors.Is(err, test.expect) {
				t.Fatalf("status=%d got %v, want %v", test.status, err, test.expect)
			}
		})
	}

	// A redirect must not be followed to another authority or path.
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/internal/v1/approval-command-prechecks", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	redirectSource, err := NewHTTPApprovalPrecheckSource(redirect.URL, "test-workload")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := redirectSource.Precheck(context.Background(), request); !errors.Is(err, ErrApprovalPrecheckUnavailable) {
		t.Fatalf("redirect must fail closed, got %v", err)
	}
	// A closed listener is unavailable, never a silent empty projection.
	server.Close()
	if _, err := source.Precheck(context.Background(), request); !errors.Is(err, ErrApprovalPrecheckUnavailable) {
		t.Fatalf("closed owner must fail closed, got %v", err)
	}
}

type approvalPrecheckSourceStub struct {
	calls      int
	request    ApprovalPrecheckRequest
	projection ApprovalPrecheckProjection
	err        error
}

func (stub *approvalPrecheckSourceStub) Precheck(_ context.Context, request ApprovalPrecheckRequest) (ApprovalPrecheckProjection, error) {
	stub.calls++
	stub.request = request
	return stub.projection, stub.err
}

func TestApprovalPrecheckProxyIsClosedAndUncacheable(t *testing.T) {
	receivableID := uuid.New()
	stub := &approvalPrecheckSourceStub{projection: ApprovalPrecheckProjection{
		CommandType: "RECEIVABLE_WRITE_OFF", PolicyVersion: 1, SnapshotSchemaVersion: 1,
		TargetResourceType: "RECEIVABLE", TargetResourceID: receivableID.String(),
		NormalizedCommand:        json.RawMessage(`{"receivable_id":"` + receivableID.String() + `"}`),
		CriticalResourceVersions: json.RawMessage(`{"receivable_version":3}`),
		FinancialSnapshot:        json.RawMessage(`{"outstanding_microcredits":500}`),
	}}
	service := NewService(nil, nil).WithApprovalPrecheckSource(stub)
	body := `{"command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"` + receivableID.String() + `","write_off_microcredits":500,"expected_version":3}`
	response := httptest.NewRecorder()
	httpserver.CorrelationHandler(http.HandlerFunc(service.adminApprovalCommandPrecheck)).
		ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/admin/v2/approval-command-prechecks", strings.NewReader(body)))
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Content-Type") != "application/json" || stub.calls != 1 {
		t.Fatalf("status=%d headers=%v calls=%d body=%s", response.Code, response.Header(), stub.calls, response.Body.String())
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(response.Body.Bytes(), &fields) != nil || len(fields) != 8 {
		t.Fatalf("proxy answer is not closed: %s", response.Body.String())
	}
	for _, key := range []string{"command_type", "policy_version", "snapshot_schema_version", "target_resource_type",
		"target_resource_id", "normalized_command", "critical_resource_versions", "financial_snapshot"} {
		if len(fields[key]) == 0 {
			t.Fatalf("proxy answer is missing %s: %s", key, response.Body.String())
		}
	}
	if stub.request.CommandType != "RECEIVABLE_WRITE_OFF" || stub.request.WriteOffMicrocredits != 500 || stub.request.ExpectedVersion != 3 {
		t.Fatalf("proxy forwarded the wrong closed request: %+v", stub.request)
	}

	// Every unusable answer keeps a distinct, stable status and never leaks the
	// owner's error text.
	for name, test := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"owner refusal":  {err: ErrApprovalPrecheckRejected, status: http.StatusUnprocessableEntity, code: "APPROVAL_PRECHECK_UNAVAILABLE"},
		"owner timeout":  {err: ErrApprovalPrecheckTimeout, status: http.StatusGatewayTimeout, code: "APPROVAL_PRECHECK_TIMEOUT"},
		"owner failed":   {err: ErrApprovalPrecheckUnavailable, status: http.StatusServiceUnavailable, code: "APPROVAL_PRECHECK_UNAVAILABLE"},
		"invalid answer": {err: ErrApprovalPrecheckInvalid, status: http.StatusServiceUnavailable, code: "APPROVAL_PRECHECK_UNAVAILABLE"},
	} {
		t.Run(name, func(t *testing.T) {
			failing := &approvalPrecheckSourceStub{err: test.err}
			handler := httpserver.CorrelationHandler(http.HandlerFunc(NewService(nil, nil).WithApprovalPrecheckSource(failing).adminApprovalCommandPrecheck))
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/v2/approval-command-prechecks", strings.NewReader(body)))
			var payload struct {
				Code string `json:"code"`
			}
			if recorder.Code != test.status || json.Unmarshal(recorder.Body.Bytes(), &payload) != nil || payload.Code != test.code ||
				strings.Contains(recorder.Body.String(), "precheck rejected") {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// Malformed transport input is refused before the owner is called at all.
	uncalled := &approvalPrecheckSourceStub{}
	handler := httpserver.CorrelationHandler(http.HandlerFunc(NewService(nil, nil).WithApprovalPrecheckSource(uncalled).adminApprovalCommandPrecheck))
	for name, raw := range map[string]string{
		"unknown field":   body[:len(body)-1] + `,"extra":1}`,
		"duplicate keys":  `{"command_type":"RECEIVABLE_WRITE_OFF","command_type":"RECEIVABLE_WRITE_OFF","target_resource_id":"` + receivableID.String() + `"}`,
		"unknown command": `{"command_type":"FUTURE_REPAIR","target_resource_id":"` + receivableID.String() + `"}`,
		"trailing json":   body + `{"command_type":"RECEIVABLE_WRITE_OFF"}`,
		"not an object":   `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/admin/v2/approval-command-prechecks", strings.NewReader(raw)))
			if recorder.Code != http.StatusBadRequest || uncalled.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, uncalled.calls, recorder.Body.String())
			}
		})
	}

	// Without a configured owner reader the endpoint is unavailable, never open.
	unwired := httptest.NewRecorder()
	httpserver.CorrelationHandler(http.HandlerFunc(NewService(nil, nil).adminApprovalCommandPrecheck)).
		ServeHTTP(unwired, httptest.NewRequest(http.MethodPost, "/admin/v2/approval-command-prechecks", strings.NewReader(body)))
	if unwired.Code != http.StatusServiceUnavailable || unwired.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unwired status=%d headers=%v", unwired.Code, unwired.Header())
	}
}
