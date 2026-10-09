package account

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApprovalProofPublicErrorClasses(t *testing.T) {
	for _, sample := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrApprovalProofRejected, 409, "APPROVAL_PROOF_REJECTED"},
		{ErrApprovalProofInvalid, 502, "APPROVAL_PROOF_INVALID"},
		{ErrApprovalProofTooLarge, 502, "APPROVAL_PROOF_TOO_LARGE"},
		{ErrApprovalProofUnavailable, 503, "APPROVAL_PROOF_UNAVAILABLE"},
		{ErrApprovalProofTimeout, 504, "APPROVAL_PROOF_TIMEOUT"},
		{ErrApprovalRequestChanged, 409, "APPROVAL_REQUEST_CHANGED"},
		{ErrApprovalRelayFenced, 503, "APPROVAL_RELAY_FENCED"},
		{ErrApprovalQuiescing, 503, "APPROVAL_QUIESCING"},
		{ErrApprovalSafeRollback, 503, "APPROVAL_SAFE_ROLLBACK"},
	} {
		recorder := httptest.NewRecorder()
		(&Service{}).writeError(recorder, httptest.NewRequest(http.MethodPost, "/account/admin/v1/approval-requests/id/actions/decide", nil), sample.err)
		if recorder.Code != sample.status || !strings.Contains(recorder.Body.String(), sample.code) ||
			strings.HasPrefix(sample.code, "APPROVAL_PROOF_") && strings.Contains(recorder.Body.String(), sample.err.Error()) ||
			recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("unclosed proof error %s: %d %s", sample.code, recorder.Code, recorder.Body.String())
		}
	}
	if errors.Is(ErrApprovalProofRejected, ErrApprovalProofInvalid) {
		t.Fatal("proof denial must remain distinct from malformed proof")
	}
}
