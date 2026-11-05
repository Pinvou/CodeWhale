package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestF03ProviderStatusCannotHideConfirmedFact(t *testing.T) {
	for _, kind := range []string{"PAYMENT", "REFUND", "CHARGEBACK"} {
		for _, status := range []string{"", " \t", "UNKNOWN", "FAILED", "SUCCESS"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				row := ProviderBillEntry{EntryType: kind, ProviderReferenceID: "controlled-reference", MerchantOrderNo: "controlled-order", AmountFen: 100, Currency: "CNY", Status: status}
				fact := ReconciliationInternalFact{EntryType: kind, ProviderReferenceID: row.ProviderReferenceID, MerchantOrderNo: row.MerchantOrderNo, PaymentOrderID: uuid.New(), AmountFen: row.AmountFen, Currency: row.Currency, LedgerPresent: true, EventDateVerified: true}
				counts := map[string]int{}
				for _, difference := range CompareFourWayReconciliation([]ProviderBillEntry{row}, []ReconciliationInternalFact{fact}) {
					counts[difference.CaseType]++
				}
				if status == "SUCCESS" {
					if len(counts) != 0 {
						t.Fatalf("confirmed supplier fact failed reconciliation: %v", counts)
					}
					return
				}
				prefix := map[string]string{"PAYMENT": "PAID", "REFUND": "REFUND", "CHARGEBACK": "CHARGEBACK"}[kind]
				if len(counts) != 2 || counts["UNSUPPORTED_PROVIDER_ENTRY"] != 1 || counts["INTERNAL_"+prefix+"_PROVIDER_MISSING"] != 1 {
					t.Fatalf("unconfirmed supplier status hid confirmed internal fact: %q %v", status, counts)
				}
			})
		}
	}
}

// Classification of the existing retry primitive used by the public command.
// Real UpdateCase transactions and handler deadlines are separately PG opt-in.
func TestF03CaseRetryDoesNotReplayUnknownOutcome(t *testing.T) {
	for name, failure := range map[string]error{
		"unknown_commit":          io.EOF,
		"provider_error":          errors.New("controlled network unavailable"),
		"cancelled":               context.Canceled,
		"ordinary_database_error": &pgconn.PgError{Code: "23505"},
	} {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			err := reconciliationRetry(context.Background(), func() error {
				attempts++
				return failure
			})
			if err != failure || attempts != 1 {
				t.Fatal("non-definite transaction failure was automatically replayed", attempts, err)
			}
		})
	}
}

// Actual HTTP row-budget behavior, with supplier capabilities controlled and no
// repository connection. No permission middleware or live vendor claim.
func TestF03PublicImportOversizedRowsKeepsExistingReceipt(t *testing.T) {
	provider := &reconciliationFixtureProvider{}
	s := NewService((*PostgresRepository)(nil)).WithPayments(&PaymentCoordinator{provider: provider})
	body := `{"channel":"ALIPAY","business_date":"2026-10-07","items":[` + strings.TrimSuffix(strings.Repeat("{},", 100001), ",") + `]}`
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-imports", strings.NewReader(body))
	out := httptest.NewRecorder()
	s.adminImportReconciliation(out, request)
	var receipt struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(out.Body.Bytes(), &receipt) != nil || out.Code != http.StatusUnprocessableEntity || receipt.Code != "PAYMENT_CAPABILITY_UNSUPPORTED" {
		t.Fatalf("existing public import budget receipt changed: %d %s", out.Code, out.Body.String())
	}
}
