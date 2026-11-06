package portal

import (
	"net/http"
	"testing"
)

func TestReconciliationRepairContextProxyReadOnly(t *testing.T) {
	const path = "/admin/v1/reconciliation-cases/01a00000-0000-7000-8000-000000000038/repair-context"
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if !billingAdminPathAllowed(method, path) {
			t.Fatalf("source read blocked: %s", method)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if billingAdminPathAllowed(method, path) {
			t.Fatalf("source path exposed mutation: %s", method)
		}
	}
	for _, invalid := range []string{
		"/admin/v1/reconciliation-cases/not-a-uuid/repair-context",
		"/admin/v1/reconciliation-cases/01a00000-0000-7000-8000-000000000038/repair-context/extra",
		"/admin/v1/reconciliation-cases/01a00000-0000-7000-8000-000000000038/payment-evidence",
		"/admin/v1/reconciliation-runs/01a00000-0000-7000-8000-000000000038/repair-context",
	} {
		if billingAdminPathAllowed(http.MethodGet, invalid) {
			t.Fatalf("unexpected source route allowed: %s", invalid)
		}
	}
}
