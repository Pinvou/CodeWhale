package portal

import (
	"net/http"
	"testing"
)

func TestLateArchiveCommandProxyProtocol(t *testing.T) {
	base := "/admin/v1/late-payment-reviews/11111111-1111-4111-8111-111111111111/"
	t.Run("new-action-is-explicitly-allowed", func(t *testing.T) {
		if !billingAdminPathAllowed(http.MethodPost, base+"archive-command") {
			t.Fatal("new archive command cannot reach Billing")
		}
	})
	t.Run("legacy-action-is-not-a-write-fallback", func(t *testing.T) {
		if billingAdminPathAllowed(http.MethodPost, base+"archive") {
			t.Fatal("legacy archive remains a write fallback")
		}
	})
	t.Run("new-action-is-post-only", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			if billingAdminPathAllowed(method, base+"archive-command") {
				t.Fatal("unexpected new archive method", method)
			}
		}
	})
	t.Run("other-installed-review-actions-are-preserved", func(t *testing.T) {
		for _, action := range []string{"claim", "query-commands", "query-command-results", "schedule", "provider-blind"} {
			if !billingAdminPathAllowed(http.MethodPost, base+action) {
				t.Fatal("unrelated review action changed", action)
			}
		}
	})
}
