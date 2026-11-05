package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type resolutionHTTPReviewRepository struct {
	Repository
	adminRepository
	key      string
	complete bool
	caseID   uuid.UUID
}

func (r *resolutionHTTPReviewRepository) PreparePaymentAnomalyResolution(_ context.Context, id, _ uuid.UUID, key, _, _ string) (PaymentAnomalyCase, bool, error) {
	if r.key != "" && r.key != key {
		return PaymentAnomalyCase{}, false, ErrConflict
	}
	r.key = key
	return PaymentAnomalyCase{ID: id, Status: "RESOLVED"}, r.complete, nil
}
func (r *resolutionHTTPReviewRepository) PaymentAnomalyDetail(context.Context, uuid.UUID) (PaymentAnomalyDetail, error) {
	return PaymentAnomalyDetail{}, ErrConflict
}

// A durable accepted command must retain its client key after a later error.
// Otherwise a valid retry creates a different key and can never finish.
func TestReviewAcceptedResolutionFailureKeepsRetryContract(t *testing.T) {
	repo := &resolutionHTTPReviewRepository{caseID: uuid.New()}
	service := NewService(repo)
	invoke := func(key string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/payment-anomalies/"+repo.caseID.String()+"/resolve", strings.NewReader(`{"resolution":"RECONCILE_REFUND","reason":"controlled provider reconciliation"}`))
		request.SetPathValue("case_id", repo.caseID.String())
		request.Header.Set("Idempotency-Key", key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		service.adminResolvePaymentAnomaly(response, request)
		return response
	}
	first := invoke("original-command-key")
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), `"code":"COMMAND_RESULT_UNKNOWN"`) {
		t.Fatalf("accepted failure lost retry contract: %d %s", first.Code, first.Body.String())
	}
	wrongKey := invoke("replacement-command-key")
	if wrongKey.Code != http.StatusConflict {
		t.Fatalf("early binding conflict was not rejected: %d", wrongKey.Code)
	}
	repo.complete = true
	replayed := invoke("original-command-key")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), repo.caseID.String()) {
		t.Fatalf("original command did not replay: %d %s", replayed.Code, replayed.Body.String())
	}
}
