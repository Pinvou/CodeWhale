package billing

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Exercise request normalization, so generated cases remain queryable while
// unknown values still fail before a repository query is built.
func TestAdminReconciliationGeneratedCaseFilters(t *testing.T) {
	for _, kind := range []string{
		"PAYMENT_LEDGER_INVALID", "REFUND_LEDGER_INVALID", "CHARGEBACK_LEDGER_INVALID",
		"PAYMENT_PACKAGE_INVALID", "REFUND_DATE_UNVERIFIED", "CHARGEBACK_DATE_UNVERIFIED",
		"PAYMENT_LEDGER_MISSING", "REFUND_AMOUNT_MISMATCH",
	} {
		t.Run(kind, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/admin/v1/reconciliation-cases?case_type="+kind, nil)
			request, ok := normalizedAdminPageRequest(w, r, adminPageReconciliationCases, "")
			if !ok || request.Filters["case_type"] != kind {
				t.Fatalf("generated case filter rejected: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, kind := range []string{"UNKNOWN_LEDGER_INVALID", "PAYMENT_DATE_UNVERIFIED"} {
		t.Run(kind, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/admin/v1/reconciliation-cases?case_type="+kind, nil)
			if _, ok := normalizedAdminPageRequest(w, r, adminPageReconciliationCases, ""); ok || w.Code != http.StatusBadRequest {
				t.Fatalf("unknown case filter accepted: %d", w.Code)
			}
		})
	}
}
