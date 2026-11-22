package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestHTTPBillingImpactCoverageUsesServiceAuthAndClosedContract(t *testing.T) {
	modelID, priceID := uuid.New(), uuid.New()
	priceVersion := int64(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/pricing/impact-coverage" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Fatal("missing service authorization")
		}
		var body struct {
			Items []ImpactPriceQuery `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Items) != 1 || body.Items[0].ModelID != modelID || body.Items[0].ProtocolCode != "CHAT" {
			t.Fatalf("body=%v err=%v", body, err)
		}
		formulaID, effectiveFrom := uuid.New(), time.Now().UTC().Add(-time.Hour)
		_ = json.NewEncoder(w).Encode(ImpactPriceCoverageResult{Source: "BILLING", AsOf: time.Now().UTC(), FinancialFactRevision: 7, PriceSetFingerprint: strings.Repeat("a", 64), Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effectiveFrom}}})
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ImpactPriceCoverage(context.Background(), []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}})
	if err != nil || len(result.Items) != 1 || !result.Items[0].Covered || result.Items[0].PriceVersionID == nil || *result.Items[0].PriceVersionID != priceID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestHTTPBillingImpactCoverageRejectsUnknownResponseFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"as_of":"2026-09-03T00:00:00Z","source":"BILLING","unexpected":"secret"}`))
	}))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "test-service-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ImpactPriceCoverage(context.Background(), []ImpactPriceQuery{{ModelID: uuid.New(), ProtocolCode: "CHAT", CreditUnit: "MICROCREDIT"}})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err=%v", err)
	}
}
