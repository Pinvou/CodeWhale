package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAdminFinancialReadRetainsExactIntegersAndSnapshots(t *testing.T) {
	raw := json.RawMessage(`{"provider_snapshot":{"paid_amount_fen":9007199254740993},"internal_snapshot":{"nested":[{"balance_microcredits":9223372036854775807}]},"refund_snapshot":{"refund_amount_fen":-9007199254740993},"ledger_snapshot":{"delta_microcredits":-9223372036854775808},"revision":17}`)
	before := append([]byte(nil), raw...)
	recorder := httptest.NewRecorder()
	writeAdminPageResponse(recorder, AdminPaginationPage{}, []any{Wallet{PostedMicrocredits: 9007199254740993}, raw})
	var response struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 {
		t.Fatal("admin page omitted financial facts")
	}
	var wallet struct {
		Posted json.RawMessage `json:"posted_microcredits"`
	}
	if err := json.Unmarshal(response.Items[0], &wallet); err != nil {
		t.Fatal(err)
	}
	if string(wallet.Posted) != `"9007199254740993"` {
		t.Fatal("admin wallet money was not a decimal string")
	}
	for _, expected := range []string{`"9007199254740993"`, `"9223372036854775807"`, `"-9007199254740993"`, `"-9223372036854775808"`, `"revision":17`} {
		if !bytes.Contains(response.Items[1], []byte(expected)) {
			t.Fatal("admin snapshot lost exact financial or nonfinancial facts")
		}
	}
	if !bytes.Equal(raw, before) {
		t.Fatal("admin read projection mutated the persisted snapshot")
	}
}

func TestFinanceOverviewMoneyIsExactWithoutChangingCounts(t *testing.T) {
	for _, unit := range []string{"FEN", "CNY_FEN", "MICROCREDITS", "COUNT"} {
		raw, err := json.Marshal(FinanceOverviewMetric{Key: "fixture", Value: 9007199254740993, Unit: unit})
		if err != nil {
			t.Fatal(err)
		}
		var metric struct {
			Value json.RawMessage `json:"value"`
		}
		if err = json.Unmarshal(raw, &metric); err != nil {
			t.Fatal(err)
		}
		expected := `"9007199254740993"`
		if unit == "COUNT" {
			expected = "9007199254740993"
		}
		if string(metric.Value) != expected {
			t.Fatal("financial metric changed precision or count contract")
		}
	}
}

// Exercise the producer's unit through its HTTP handler rather than only the
// metric serializer. Repository values are controlled; no database is claimed.
func TestPaymentsOverviewRetainsExactMoneyAndNumericCounts(t *testing.T) {
	s := &Service{repository: exactFinanceOverviewRepository{}, now: func() time.Time {
		return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	}}
	w := httptest.NewRecorder()
	s.adminFinancePaymentsOverview(w, httptest.NewRequest(http.MethodGet, "/admin/v1/finance-overviews/payments", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("payments overview status %d", w.Code)
	}
	var result struct {
		Metrics []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
			Unit  string          `json:"unit"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	moneyFound, countFound := false, false
	for _, metric := range result.Metrics {
		switch metric.Key {
		case "payments_received_fen":
			moneyFound = true
			if metric.Unit != "CNY_FEN" || string(metric.Value) != `"9007199254740993"` {
				t.Fatalf("payment money lost its unit or exact digits: %s %s", metric.Unit, metric.Value)
			}
		case "orders_created_count":
			countFound = true
			if metric.Unit != "COUNT" || string(metric.Value) != "2" {
				t.Fatalf("order count changed its number contract: %s %s", metric.Unit, metric.Value)
			}
		}
	}
	if !moneyFound || !countFound {
		t.Fatal("payments overview omitted money or count metric")
	}
}

type exactFinanceOverviewRepository struct{ Repository }

func (exactFinanceOverviewRepository) AdminFinanceAccountsOverview(context.Context, time.Time) (financeAccountsOverviewValues, error) {
	return financeAccountsOverviewValues{}, nil
}
func (exactFinanceOverviewRepository) AdminFinancePaymentsOverview(context.Context, time.Time, time.Time, time.Time) (financePaymentsOverviewValues, error) {
	return financePaymentsOverviewValues{PaymentsReceivedFen: 9007199254740993, OrdersCreatedCount: 2}, nil
}
func (exactFinanceOverviewRepository) AdminFinanceProductsOverview(context.Context, time.Time) (financeProductsOverviewValues, error) {
	return financeProductsOverviewValues{}, nil
}
