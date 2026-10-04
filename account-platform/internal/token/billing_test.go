package token

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestCostUsesIntegerCeilingAndCachedRate(t *testing.T) {
	price := PriceVersion{InputPerMillion: 1_000_000, OutputPerMillion: 2_000_000, CachedInputPerMillion: 100_000}
	cost, err := Cost(price, Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 7 {
		t.Fatalf("Cost = %d, want 7", cost)
	}
}

func TestCostRejectsInvalidUsage(t *testing.T) {
	if _, err := Cost(PriceVersion{}, Usage{InputTokens: 1, CachedInputTokens: 2}); err == nil {
		t.Fatal("cached tokens greater than input were accepted")
	}
}

func TestMaximumCostBoundsCachedPricesAndSplitCeilings(t *testing.T) {
	for _, price := range []PriceVersion{{InputPerMillion: 1, CachedInputPerMillion: 1}, {InputPerMillion: 2, CachedInputPerMillion: 10}, {InputPerMillion: 1_000_000, CachedInputPerMillion: 2_000_000, OutputPerMillion: 1_000_000}, {InputPerMillion: 0, CachedInputPerMillion: 100_000}, {InputPerMillion: 100_001, CachedInputPerMillion: 99_999, OutputPerMillion: 20_000}} {
		bound, err := MaximumCost(price, 100, 3)
		if err != nil {
			t.Fatal(err)
		}
		for input := int64(0); input <= 100; input++ {
			for cached := int64(0); cached <= input; cached++ {
				actual, err := Cost(price, Usage{InputTokens: input, CachedInputTokens: cached, OutputTokens: 3})
				if err != nil || actual > bound {
					t.Fatalf("bound=%d actual=%d input=%d cached=%d price=%+v err=%v", bound, actual, input, cached, price, err)
				}
			}
		}
	}
}

func TestRecentRechargeOrderIsScopedToRequesterAndWallet(t *testing.T) {
	requesterID := uuid.New()
	walletID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/recharge/orders/recent" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("requester_id") != requesterID.String() || r.URL.Query().Get("wallet_id") != walletID.String() {
			t.Fatalf("recent lookup was not scoped to requester and wallet: %q", r.URL.RawQuery)
		}
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatal("missing service authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"order":null}`))
	}))
	defer server.Close()

	client, err := NewHTTPBillingClient(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := client.RecentRechargeOrder(context.Background(), requesterID, walletID)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"order":null}` {
		t.Fatalf("unexpected payload %s", payload)
	}
}
