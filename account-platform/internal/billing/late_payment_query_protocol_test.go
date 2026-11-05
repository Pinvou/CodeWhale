package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type lateQueryProtocolRepository struct {
	Repository
	starts, reads int
}

func (r *lateQueryProtocolRepository) PrepareLatePaymentQuery(_ context.Context, id, actor uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentQueryReceipt, error) {
	r.starts++
	return LatePaymentQueryReceipt{CommandProtocol: 1, Reference: key, CaseID: id, ActorID: actor, PaymentOrderID: input.PaymentOrderID, State: "UNKNOWN"}, nil
}
func (r *lateQueryProtocolRepository) ReadLatePaymentQuery(_ context.Context, id, actor uuid.UUID, key string, input LatePaymentQueryInput) (LatePaymentQueryReceipt, error) {
	r.reads++
	return LatePaymentQueryReceipt{CommandProtocol: 1, Reference: key, CaseID: id, ActorID: actor, PaymentOrderID: input.PaymentOrderID, State: "UNKNOWN"}, nil
}
func (r *lateQueryProtocolRepository) CompleteLatePaymentQuery(context.Context, uuid.UUID, uuid.UUID, string, LatePaymentQueryInput, PaymentOrder, error) (LatePaymentQueryReceipt, error) {
	panic("unexpected complete")
}

func lateQueryAuthorizedHandler(t *testing.T, s *Service, actor uuid.UUID, allowed *bool) http.Handler {
	t.Helper()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Permission string `json:"permission"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Permission != "platform.billing.orders.sync" {
			t.Error("query changed its current permission")
		}
		w.Header().Set("X-Authorization-Version", "1")
		json.NewEncoder(w).Encode(map[string]any{"allowed": *allowed, "user_id": actor})
	}))
	t.Cleanup(auth.Close)
	client, err := authorization.NewClient(auth.URL, "PUBLIC_SYNTHETIC_INTERNAL_QUERY")
	if err != nil {
		t.Fatal(err)
	}
	s.WithAuthorizer(client)
	return s.Handler("PUBLIC_SYNTHETIC_INTERNAL_QUERY", func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "controlled", Subject: actor.String(), SessionID: "controlled", AuthenticatedAt: time.Now()}, nil
	})
}

func TestLatePaymentQueryClosedHTTPRoutes(t *testing.T) {
	id, actor := uuid.New(), uuid.New()
	input := LatePaymentQueryInput{1, uuid.New(), "controlled", "v1", "controlled-order"}
	raw, _ := json.Marshal(input)
	repository := &lateQueryProtocolRepository{}
	provider := &lateQueryTestProvider{}
	s := NewService(repository).WithPayments(&PaymentCoordinator{provider: provider})
	allowed := true
	handler := lateQueryAuthorizedHandler(t, s, actor, &allowed)
	for _, action := range []string{"query-commands", "query-command-results"} {
		t.Run(action, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/admin/v1/late-payment-reviews/"+id.String()+"/"+action, strings.NewReader(string(raw)))
			request.Header.Set("Authorization", "Bearer PUBLIC_SYNTHETIC_QUERY")
			request.Header.Set("Idempotency-Key", "controlled-query-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 || response.Header().Get("X-Command-Type") != "BILLING_LATE_PAYMENT_QUERY" {
				t.Fatal("new route not governed by original current policy", response.Code, response.Body.String())
			}
		})
	}
	t.Run("legacy_keyless_route_refuses_before_any_repository_or_Provider", func(t *testing.T) {
		before := repository.starts + repository.reads
		request := httptest.NewRequest("POST", "/admin/v1/late-payment-reviews/"+id.String()+"/query", strings.NewReader("{}"))
		request.Header.Set("Authorization", "Bearer PUBLIC_SYNTHETIC_QUERY")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 409 || !strings.Contains(response.Body.String(), "QUERY_PROTOCOL_REQUIRED") || repository.starts+repository.reads != before || provider.gets.Load() != 0 || provider.closes.Load() != 0 {
			t.Fatal("legacy query dispatched", response.Code)
		}
	})
	t.Run("revoked_permission_refuses_start_and_result_before_repository", func(t *testing.T) {
		allowed = false
		before := repository.starts + repository.reads
		for _, action := range []string{"query-commands", "query-command-results"} {
			request := httptest.NewRequest("POST", "/admin/v1/late-payment-reviews/"+id.String()+"/"+action, strings.NewReader(string(raw)))
			request.Header.Set("Authorization", "Bearer PUBLIC_SYNTHETIC_QUERY")
			request.Header.Set("Idempotency-Key", "controlled-query-key")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 403 {
				t.Fatal("current permission did not gate query", response.Code)
			}
		}
		if repository.starts+repository.reads != before {
			t.Fatal("revoked permission read or admitted query")
		}
	})
}

func TestLatePaymentQueryClosedDecoder(t *testing.T) {
	valid := `{"command_protocol":1,"payment_order_id":"11111111-1111-4111-8111-111111111111","provider_id":"controlled","provider_version":"v1","merchant_order_no":"controlled-order"}`
	if _, err := decodeLatePaymentQueryInput(strings.NewReader(valid)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, strings.Replace(valid, `"command_protocol":1`, `"command_protocol":1,"command_protocol":1`, 1), strings.Replace(valid, `"command_protocol":1`, `"command_protocol":2`, 1), strings.Replace(valid, `"command_protocol":1`, `"command_protocol":1,"unexpected":true`, 1), valid + `{}`, strings.Repeat(" ", 8193)} {
		if _, err := decodeLatePaymentQueryInput(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid command admitted", raw[:min(len(raw), 100)])
		}
	}
}
