package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDirectPaymentRepairProxyPreservesProtocolKeyAndSession(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	body := `{"reason":"controlled original direct repair","command_protocol":1}`
	receipt := `{"order_id":"` + id + `","status":"PAID","fulfillment_status":"CREDITED"}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != body || r.Method != "POST" || r.URL.Path != "/admin/v1/payment-orders/"+id+"/repair" || r.Header.Get("Idempotency-Key") != "controlled-direct-repair-key" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("X-CSRF-Token") != "" {
			t.Error("repair request protocol, original identity or session authority changed")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, receipt)
	}))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	for range 2 {
		request := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/payment-orders/"+id+"/repair", body)
		request.Header.Set("Authorization", "Bearer untrusted-browser-token")
		request.Header.Set("Idempotency-Key", "controlled-direct-repair-key")
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != 200 || response.Body.String() != receipt {
			t.Fatal("repair receipt changed across proxy", response.Code, response.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatal("explicit retry count changed", calls.Load())
	}
	request := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/payment-orders/"+id+"/repair", body)
	request.Header.Set("X-CSRF-Token", "wrong")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != 403 || calls.Load() != 2 {
		t.Fatal("invalid CSRF reached Billing", response.Code, calls.Load())
	}
}
