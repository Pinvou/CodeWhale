package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Controlled HTTP responses exercise the production client, not a live Billing service.
func TestBillingHoldResponsesRequireAuthoritativeFacts(t *testing.T) {
	usage, hold, wallet, price := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, operation := range []string{"reserve", "commit", "release"} {
		for _, scenario := range []string{"valid", "empty", "html", "created", "accepted", "trailing", "oversize", "wrong-hold", "wrong-usage", "wrong-wallet", "wrong-state", "wrong-actual", "missing-maximum", "wrong-price", "wrong-maximum"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				body := map[string]any{"hold_id": hold, "usage_request_id": usage, "wallet_id": wallet, "price_version_id": price, "max_microcredits": int64(10), "expires_at": time.Now().Add(time.Hour), "status": "ACTIVE"}
				if operation == "commit" {
					body["status"] = "COMMITTED"
					body["actual_microcredits"] = int64(3)
				}
				if operation == "release" {
					body["status"] = "RELEASED"
				}
				switch scenario {
				case "wrong-hold":
					body["hold_id"] = uuid.Nil
				case "wrong-usage":
					body["usage_request_id"] = uuid.New()
				case "wrong-wallet":
					body["wallet_id"] = uuid.New()
				case "wrong-state":
					body["status"] = "REVIEW"
				case "wrong-actual":
					body["actual_microcredits"] = int64(4)
				case "missing-maximum":
					delete(body, "max_microcredits")
				case "wrong-price":
					body["price_version_id"] = uuid.New()
				case "wrong-maximum":
					body["max_microcredits"] = int64(2)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if scenario == "created" {
						w.WriteHeader(201)
					}
					if scenario == "accepted" {
						w.WriteHeader(202)
					}
					if scenario == "empty" {
						return
					}
					if scenario == "html" {
						_, _ = w.Write([]byte("<html>ok</html>"))
						return
					}
					_ = json.NewEncoder(w).Encode(body)
					if scenario == "trailing" {
						_, _ = w.Write([]byte("{}"))
					}
					if scenario == "oversize" {
						_, _ = w.Write([]byte(strings.Repeat(" ", 2<<20)))
					}
				}))
				defer server.Close()
				client, err := NewHTTPBillingClient(server.URL, "fixture-service-token")
				if err != nil {
					t.Fatal(err)
				}
				switch operation {
				case "reserve":
					_, err = client.Reserve(context.Background(), usage, wallet, 10, price)
				case "commit":
					err = client.Commit(context.Background(), usage, hold, wallet, 3, Usage{InputTokens: 3})
				case "release":
					err = client.Release(context.Background(), usage, hold, wallet, "known unused")
				}
				wantValid := scenario == "valid" || (scenario == "wrong-price" && operation != "reserve") || (scenario == "wrong-maximum" && operation == "release")
				if (err == nil) != wantValid {
					t.Fatalf("response accepted=%v, expected=%v", err == nil, wantValid)
				}
			})
		}
	}
}

func TestBillingClientRejectsUnsafeConfigurationAndRedirect(t *testing.T) {
	for _, address := range []string{"file:///billing", "ftp://example.test", "http://user:password@example.test", "http://example.test?route=other", "http://example.test#other"} {
		if _, err := NewHTTPBillingClient(address, "fixture-token"); err == nil {
			t.Fatal("unsafe configuration was accepted")
		}
	}
	var calls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewHTTPBillingClient(redirect.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Commit(context.Background(), uuid.New(), uuid.New(), uuid.New(), 1, Usage{InputTokens: 1}); err == nil {
		t.Fatal("redirect accepted as settlement")
	}
	if _, err = client.Products(context.Background()); err == nil {
		t.Fatal("redirect accepted as catalog")
	}
	if calls.Load() != 0 {
		t.Fatal("Billing redirected credentials or financial command")
	}
}

func TestBillingPriceResponseMatchesRequestedFacts(t *testing.T) {
	priceID := uuid.New()
	for _, operation := range []string{"current", "historical"} {
		for _, scenario := range []string{"valid-zero", "wrong-id", "wrong-model", "missing-rate", "negative-rate", "null"} {
			t.Run(operation+"/"+scenario, func(t *testing.T) {
				body := map[string]any{"price_version_id": priceID, "model": "fixture-model", "input_per_million": 0, "output_per_million": 0, "cached_input_per_million": 0}
				switch scenario {
				case "wrong-id":
					if operation == "current" {
						body["price_version_id"] = uuid.Nil
					} else {
						body["price_version_id"] = uuid.New()
					}
				case "wrong-model":
					if operation == "current" {
						body["model"] = "other-model"
					} else {
						body["model"] = ""
					}
				case "missing-rate":
					delete(body, "output_per_million")
				case "negative-rate":
					body["input_per_million"] = -1
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if scenario == "null" {
						_, _ = w.Write([]byte("null"))
						return
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				client, err := NewHTTPBillingClient(server.URL, "fixture-token")
				if err != nil {
					t.Fatal(err)
				}
				if operation == "current" {
					_, err = client.Price(context.Background(), "fixture-model")
				} else {
					_, err = client.PriceByID(context.Background(), priceID)
				}
				if (err == nil) != (scenario == "valid-zero") {
					t.Fatal("price response did not match authoritative request facts")
				}
			})
		}
	}
}

// Token has real PG state; deliberately malformed Billing replies are controlled
// HTTP fixtures and do not claim a real remote financial operation occurred.
func TestPostgresMalformedBillingReplyNeverCommitsTokenTerminal(t *testing.T) {
	for _, operation := range []string{"commit", "release"} {
		for _, reply := range []string{"html", "empty", "accepted", "wrong-wallet"} {
			t.Run(operation+"/"+reply, func(t *testing.T) {
				ctx := context.Background()
				repo, usage := settlementFixture(t)
				stored, err := repo.GetUsage(ctx, usage.ID)
				if err != nil || stored.HoldID == nil {
					t.Fatal("usage fixture missing")
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if reply == "accepted" {
						w.WriteHeader(http.StatusAccepted)
						return
					}
					if reply == "empty" {
						return
					}
					if reply == "html" {
						_, _ = w.Write([]byte("<html>ok</html>"))
						return
					}
					status := "COMMITTED"
					if operation == "release" {
						status = "RELEASED"
					}
					body := map[string]any{"hold_id": *stored.HoldID, "usage_request_id": usage.ID, "wallet_id": uuid.New(), "status": status, "max_microcredits": 100, "expires_at": time.Now().Add(time.Hour)}
					if operation == "commit" {
						body["actual_microcredits"] = 3
					}
					_ = json.NewEncoder(w).Encode(body)
				}))
				defer server.Close()
				client, err := NewHTTPBillingClient(server.URL, "fixture-token")
				if err != nil {
					t.Fatal(err)
				}
				s := &Service{repository: repo, billing: client}
				if operation == "commit" {
					s.settle(ctx, usage.ID, *stored.HoldID, 100, PriceVersion{InputPerMillion: 1_000_000}, Usage{InputTokens: 3})
				} else {
					s.releaseUnused(ctx, usage.ID, *stored.HoldID, "known_unused")
				}
				stored, err = repo.GetUsage(ctx, usage.ID)
				wantStatus := "SETTLEMENT_PENDING"
				if operation == "release" {
					wantStatus = "REVIEW"
				}
				if err != nil || stored.Status != wantStatus || stored.ActualMicrocredits != nil {
					t.Fatal("unproven Billing response created a Token terminal")
				}
				want := "settlement_pending"
				if operation == "release" {
					want = "release_pending"
				}
				if stored.ErrorClass == nil || *stored.ErrorClass != want {
					t.Fatal("unconfirmed command did not retain its recovery intent")
				}
			})
		}
	}
}
