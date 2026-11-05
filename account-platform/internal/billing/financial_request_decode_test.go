package billing

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Exercise installed handlers after their authentication boundary. A repository
// without a pool makes accidental mutation fail; malformed input must stop at
// decoding and explicitly return400, never an empty success response.
func TestFinancialHandlersRejectMalformedRequests(t *testing.T) {
	s := NewService(&PostgresRepository{})
	handlers := map[string]http.HandlerFunc{
		"create-product":        s.adminCreateRechargeProduct,
		"update-draft":          s.adminUpdateRechargeProductDraft,
		"publish":               s.adminPublishRechargeProduct,
		"unpublish":             s.adminUnpublishRechargeProduct,
		"delete-product":        s.adminDeleteRechargeProduct,
		"display-names":         s.adminUpdateRechargeProductDisplayNames,
		"start-verification":    s.adminStartProductAdmissionVerification,
		"retry-verification":    s.adminRetryProductAdmissionVerification,
		"revoke-admission":      s.adminRevokeProductAdmission,
		"complete-verification": s.completeProductAdmissionVerification,
		"provider-revoke":       s.revokeProductAdmissionInternal,
		"schedule-review":       s.adminScheduleLatePaymentReview,
		"provider-blind":        s.adminEscalateProviderBlind,
	}
	for name, handler := range handlers {
		for fixture, body := range map[string]string{"truncated": "{", "unknown-field": `{"unrecognized":true}`, "second-document": `{} {}`} {
			t.Run(name+"/"+fixture, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "/controlled-decoder", strings.NewReader(body))
				r.Header.Set("Idempotency-Key", "owned-decode-request-identity")
				for _, key := range []string{"catalog_item_id", "verification_id", "evidence_id", "case_id"} {
					r.SetPathValue(key, "11111111-1111-4111-8111-111111111111")
				}
				w := httptest.NewRecorder()
				handler(w, r)
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `"code":"INVALID_REQUEST"`) {
					t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
				}
			})
		}
	}
}
