package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func financeBrowserRequest(service *Service, session Session, method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Origin", service.config.PublicURL.String())
	request.Header.Set("X-CSRF-Token", session.CSRFToken)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: service.sessionCookieName(), Value: session.ID})
	request.AddCookie(&http.Cookie{Name: service.csrfCookieName(), Value: session.CSRFToken})
	return request
}

func TestFinanceProxyPreservesWholeRefundBytesAndSessionAuthority(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	body := `{"payment_transaction_id":"` + id + `","entitlement_package_id":"` + id + `","reason":"controlled whole refund"}`
	receipt := `{"refund_id":"` + id + `","amount_fen":9007199254740993}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != body || r.URL.Path != "/admin/v1/payment-orders/"+id+"/refunds" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Idempotency-Key") != "controlled-whole-refund-key" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("X-Pinvou-Approval-Proof-Test") != "" {
			t.Error("refund request facts or server authority changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, receipt)
	}))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	request := financeBrowserRequest(service, session, http.MethodPost, "/api/admin/billing/admin/v1/payment-orders/"+id+"/refunds", body)
	request.Header.Set("Authorization", "Bearer browser-untrusted")
	request.Header.Set("Idempotency-Key", "controlled-whole-refund-key")
	request.Header.Set("X-Pinvou-Approval-Proof-Test", "untrusted")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Body.String() != receipt || calls.Load() != 1 {
		t.Fatal("whole refund did not preserve actual upstream receipt")
	}
	rejected := financeBrowserRequest(service, session, http.MethodPost, request.URL.Path, body)
	rejected.Header.Set("X-CSRF-Token", "wrong")
	response = httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, rejected)
	if response.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatal("invalid CSRF reached Billing")
	}
}

func TestFinanceProxyRejectsFutureAndMalformedPathsBeforeUpstream(t *testing.T) {
	service := testService(t, newMemoryStore())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	for _, path := range []string{"/admin/v1/reconciliation-cases", "/admin/v1/usage-holds", "/admin/v1/account-closed-grants", "/admin/v1/payment-orders/not-a-uuid/refunds", "/admin/v1/payment-orders/00000000-0000-0000-0000-000000000000/refunds"} {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/billing"+path, nil)
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("future/malformed path returned %d", response.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected path reached upstream")
	}
}

type financeWireStore struct {
	*memoryStore
	muWire sync.Mutex
	wire   map[string]int
}

func (s *financeWireStore) ApprovalWireVersion(_ context.Context, session Session) (int, error) {
	s.muWire.Lock()
	defer s.muWire.Unlock()
	if value, ok := s.wire[session.ID]; ok {
		return value, nil
	}
	return 1, nil
}

func (s *financeWireStore) LockApprovalWireVersion(_ context.Context, session Session, version int) (int, error) {
	s.muWire.Lock()
	defer s.muWire.Unlock()
	if value, ok := s.wire[session.ID]; ok {
		return value, nil
	}
	s.wire[session.ID] = version
	return version, nil
}

func TestFinanceApprovalMutationRequiresExactCurrentAcknowledgement(t *testing.T) {
	id := uuid.NewString()
	for _, test := range []struct {
		name     string
		status   int
		body     string
		expected int
	}{
		{"exact200", 200, `{"approval_request_id":"` + id + `"}`, 204}, {"actual204", 204, "", 204},
		{"unconfirmed202", 202, `{"approval_request_id":"` + id + `"}`, 503}, {"wrongidentity", 200, `{"approval_request_id":"` + uuid.NewString() + `"}`, 503}, {"null", 200, "null", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var mutationCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled"}`)
					return
				}
				mutationCalls.Add(1)
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Path != "/admin/v2/approval-requests/"+id+"/actions/submit" || string(raw) != `{"expected_version":1}` || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
					t.Error("current typed mutation changed")
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, http.MethodPost, "/api/admin/approvals/requests/"+id+"/actions/submit", `{"expected_version":1}`)
			request.Header.Set("X-Pinvou-Approval-Action-Control-Version", "1")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.expected || mutationCalls.Load() != 1 {
				t.Fatalf("mutation acknowledgement status %d, calls %d", response.Code, mutationCalls.Load())
			}
		})
	}
}
