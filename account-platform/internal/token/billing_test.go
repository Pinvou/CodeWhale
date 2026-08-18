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

func TestHistoricalPriceLookupUsesImmutableIdentifier(t *testing.T) {
	priceID := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/pricing/"+priceID.String() || r.URL.RawQuery != "" {
			t.Fatalf("unexpected historical price lookup %q", r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatal("missing service authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"price_version_id":"` + priceID.String() + `","model":"example-model","input_per_million":1,"output_per_million":2,"cached_input_per_million":0}`))
	}))
	defer server.Close()

	client, err := NewHTTPBillingClient(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	price, err := client.PriceByID(context.Background(), priceID)
	if err != nil {
		t.Fatal(err)
	}
	if price.ID != priceID || price.Model != "example-model" {
		t.Fatalf("unexpected historical price: %#v", price)
	}
}
