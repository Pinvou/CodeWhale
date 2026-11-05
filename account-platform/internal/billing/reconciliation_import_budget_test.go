package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Invalid input must produce a bounded, explicit response before touching the
// repository or supplier. A typed nil repository makes accidental calls fail.
func TestF03ReconciliationMalformedInputHasErrorReceipt(t *testing.T) {
	s := NewService((*PostgresRepository)(nil)).WithPayments(&PaymentCoordinator{})
	for name, handler := range map[string]http.HandlerFunc{
		"import":   s.adminImportReconciliation,
		"manual":   s.adminRunReconciliation,
		"internal": s.runReconciliation,
		"case":     s.adminUpdateReconciliationCase,
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/controlled", strings.NewReader("{"))
			request.SetPathValue("case_id", uuid.NewString())
			request.Header.Set("Idempotency-Key", "controlled-invalid-body")
			out := httptest.NewRecorder()
			handler(out, request)
			var receipt map[string]any
			if out.Code != http.StatusBadRequest || json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt["code"] == nil {
				t.Fatalf("invalid body lacks explicit error: status=%d body=%s", out.Code, out.Body.String())
			}
		})
	}
}

func TestF03ReconciliationImportAcceptsPublicRowBudget(t *testing.T) {
	s := NewService((*PostgresRepository)(nil)).WithPayments(&PaymentCoordinator{})
	const row = `{"merchant_order_no":"controlled-import-budget-order","channel_transaction_id":"controlled-transaction","channel":"ALIPAY","amount_fen":100,"currency":"CNY","status":"SUCCESS","paid_at":"2026-10-07T00:00:00Z"}`
	body := `{"channel":"ALIPAY","business_date":"invalid-controlled-date","items":[` + strings.TrimSuffix(strings.Repeat(row+",", 100000), ",") + `]}`
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-imports", strings.NewReader(body))
	out := httptest.NewRecorder()
	s.adminImportReconciliation(out, request)
	var receipt map[string]any
	if out.Code != http.StatusBadRequest || json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt["code"] != "INVALID_REQUEST" || !strings.Contains(out.Body.String(), "business_date") {
		t.Fatalf("public 100000-row body did not reach date validation: status=%d body=%s", out.Code, out.Body.String())
	}
}

func TestF03ReconciliationImportBodyBudgetIsBounded(t *testing.T) {
	s := NewService((*PostgresRepository)(nil)).WithPayments(&PaymentCoordinator{})
	body := `{"channel":"ALIPAY","business_date":"invalid-controlled-date","items":[]}` + strings.Repeat(" ", 64<<20)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-imports", strings.NewReader(body))
	out := httptest.NewRecorder()
	s.adminImportReconciliation(out, request)
	var receipt map[string]any
	if out.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(out.Body.Bytes(), &receipt) != nil || receipt["code"] == nil {
		t.Fatalf("oversized import lacks 413 error receipt: status=%d", out.Code)
	}
}

func TestF03ReconciliationOrdinaryRequestsKeepSmallBudget(t *testing.T) {
	s := NewService((*PostgresRepository)(nil)).WithPayments(&PaymentCoordinator{})
	body := `{"channel":"ALIPAY","business_date":"invalid-controlled-date"}` + strings.Repeat(" ", 1<<20)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/reconciliation-runs", strings.NewReader(body))
	out := httptest.NewRecorder()
	s.adminRunReconciliation(out, request)
	if out.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("ordinary request budget changed or failed silently: status=%d", out.Code)
	}
}
