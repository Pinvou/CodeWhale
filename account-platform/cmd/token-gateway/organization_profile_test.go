package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Exercise the real personal production constructor and router. Every
// organization entry must stop before Account or Billing RPCs.
func TestF06PersonalGatewayDoesNotExposeOrganizationFacilities(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer peer.Close()
	service, err := newPersonalGatewayService(peer.URL, peer.URL, "controlled-workload")
	if err != nil {
		t.Fatal(err)
	}
	handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "owned", Subject: "owned-user"}, nil
	})
	base := "/v1/organizations/" + uuid.NewString()
	for _, route := range []struct{ method, suffix string }{
		{http.MethodGet, "/model-policy"},
		{http.MethodPut, "/model-policy"},
		{http.MethodGet, "/balance"},
		{http.MethodGet, "/entitlement-packages"},
		{http.MethodGet, "/recharge/products"},
		{http.MethodGet, "/payment-capabilities"},
		{http.MethodPost, "/recharge/orders"},
		{http.MethodGet, "/recharge/orders/" + uuid.NewString()},
		{http.MethodGet, "/quota-grants"},
		{http.MethodPost, "/quota-grants:batch"},
		{http.MethodPost, "/quota-grants/" + uuid.NewString() + "/revoke"},
	} {
		t.Run(route.method+route.suffix, func(t *testing.T) {
			request := httptest.NewRequest(route.method, base+route.suffix, strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer owned-user")
			request.Header.Set("Idempotency-Key", "owned-organization-request")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusNotFound || calls.Load() != 0 {
				t.Fatalf("personal exposed organization route: HTTP=%d authority calls=%d", response.Code, calls.Load())
			}
		})
	}
}
