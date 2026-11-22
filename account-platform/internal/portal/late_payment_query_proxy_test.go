package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestLatePaymentQueryProxyClosedWire(t *testing.T) {
	store := newMemoryStore()
	service := testService(t, store)
	session := validSession(uuid.NewString())
	if err := store.SaveSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	body := `{"command_protocol":1,"payment_order_id":"` + id + `","provider_id":"controlled","provider_version":"v1","merchant_order_no":"controlled-order"}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil || string(raw) != body || r.Header.Get("Idempotency-Key") != "controlled-query-key" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken {
			t.Error("Portal changed original query bytes/key/current session authority")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"state":"UNKNOWN"}`)
	}))
	defer upstream.Close()
	service.config.BillingURL = parseTestURL(t, upstream.URL)
	for _, action := range []string{"query-commands", "query-command-results"} {
		t.Run(action, func(t *testing.T) {
			request := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/late-payment-reviews/"+id+"/"+action, body)
			request.Header.Set("Idempotency-Key", "controlled-query-key")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != 200 || response.Body.String() != `{"state":"UNKNOWN"}` {
				t.Fatal("exact query action not forwarded", response.Code)
			}
		})
	}
	before := calls.Load()
	for _, action := range []string{"query", "query-command-results/extra", "query-command-retry", "query-commands/extra"} {
		request := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/late-payment-reviews/"+id+"/"+action, body)
		response := httptest.NewRecorder()
		service.Handler(nil).ServeHTTP(response, request)
		if response.Code != 404 || calls.Load() != before {
			t.Fatal("unregistered query variant reached upstream")
		}
	}
	request := financeBrowserRequest(service, session, "POST", "/api/admin/billing/admin/v1/late-payment-reviews/"+id+"/query-command-results", body)
	request.Header.Set("X-CSRF-Token", "wrong")
	response := httptest.NewRecorder()
	service.Handler(nil).ServeHTTP(response, request)
	if response.Code != 403 || calls.Load() != before {
		t.Fatal("result lookup bypassed session/CSRF boundary")
	}
}
