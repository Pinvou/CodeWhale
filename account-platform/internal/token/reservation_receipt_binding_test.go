package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestReservationHTTPReceiptsBindOriginalUsageIdentity(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	for _, method := range []string{"personal", "personal-priced", "member", "member-priced"} {
		t.Run(method, func(t *testing.T) {
			usage, wallet, price, organization, member := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			pricing := HoldPricingContext{ModelID: "controlled", ProtocolCode: "CHAT", FormulaVersionID: uuid.New(), PricingSnapshot: json.RawMessage(`{}`), RequestSnapshot: json.RawMessage(`{}`)}
			for _, scenario := range []string{"active", "released", "committed", "foreign-usage", "missing-usage", "missing-hold", "invalid-json", "rate-limited"} {
				t.Run(scenario, func(t *testing.T) {
					expected := BillingHold{ID: uuid.New(), UsageRequestID: usage, Status: "ACTIVE"}
					if scenario == "released" {
						expected.Status = "RELEASED"
					} else if scenario == "committed" {
						expected.Status = "COMMITTED"
					}
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var request map[string]json.RawMessage
						if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/holds/reserve" || json.NewDecoder(r.Body).Decode(&request) != nil || string(request["usage_request_id"]) != `"`+usage.String()+`"` || string(request["wallet_id"]) != `"`+wallet.String()+`"` {
							t.Error("reservation request lost its frozen identity")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						if scenario == "rate-limited" {
							w.WriteHeader(http.StatusTooManyRequests)
							_, _ = w.Write([]byte(`{"code":"RATE_LIMITED"}`))
							return
						}
						if scenario == "invalid-json" {
							_, _ = w.Write([]byte(`{"hold_id":`))
							return
						}
						// Real Billing returns these named fields, including wallet
						// and status for an already terminal idempotent receipt.
						body := map[string]any{"hold_id": expected.ID, "usage_request_id": usage, "wallet_id": wallet, "status": expected.Status}
						switch scenario {
						case "foreign-usage":
							body["usage_request_id"] = uuid.New()
						case "missing-usage":
							delete(body, "usage_request_id")
						case "missing-hold":
							delete(body, "hold_id")
						}
						_ = json.NewEncoder(w).Encode(body)
					}))
					defer server.Close()
					client, err := NewHTTPBillingClient(server.URL, "controlled-reservation-receipt")
					if err != nil {
						t.Fatal(err)
					}
					var hold BillingHold
					switch method {
					case "personal":
						hold, err = client.Reserve(context.Background(), usage, wallet, 100, price)
					case "personal-priced":
						hold, err = client.ReservePriced(context.Background(), usage, wallet, 100, price, pricing)
					case "member":
						hold, err = client.ReserveMemberQuota(context.Background(), usage, wallet, organization, member, 1, 100, price)
					case "member-priced":
						hold, err = client.ReserveMemberQuotaPriced(context.Background(), usage, wallet, organization, member, 1, 100, price, pricing)
					}
					if scenario == "active" || scenario == "released" || scenario == "committed" {
						if err != nil || hold != expected {
							t.Fatalf("matching idempotent receipt rejected: hold=%+v err=%v", hold, err)
						}
						return
					}
					if err == nil || hold != (BillingHold{}) {
						t.Fatal("unconfirmed or foreign receipt escaped the HTTP client")
					}
					if definitiveInitialBillingFailure(err) {
						t.Fatal("receipt protocol failure became terminal funds evidence")
					}
					if scenario == "rate-limited" {
						var failure *BillingHTTPError
						if !errors.As(err, &failure) || failure.Status != http.StatusTooManyRequests {
							t.Fatal("transport failure identity was replaced by receipt validation")
						}
					}
				})
			}
		})
	}
}
