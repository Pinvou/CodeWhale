package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type billingApprovalEffectStub struct {
	observed bool
	at       time.Time
	calls    int
}

func (stub *billingApprovalEffectStub) ObserveApprovalEffect(_ context.Context, _ uuid.UUID, _ string) (bool, time.Time, error) {
	stub.calls++
	return stub.observed, stub.at, nil
}

func TestBillingApprovalEffectObservationIsClosedReadOnlyAuthority(t *testing.T) {
	requestID := uuid.Must(uuid.NewV7())
	fingerprint := strings.Repeat("a", 64)
	reader := &billingApprovalEffectStub{at: time.Now().UTC()}
	service := (&Service{}).WithApprovalEffectReader(reader)
	ctx, err := serviceauth.ContextForJob(context.Background(), "account-service", "APPROVAL_RECONCILIATION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/approval-effects/"+requestID.String()+"?command_type=WALLET_ADJUSTMENT&command_fingerprint="+fingerprint, nil).WithContext(ctx)
	request.SetPathValue("approval_request_id", requestID.String())
	response := httptest.NewRecorder()
	service.approvalEffectObservation(response, request)
	var observation approvaleffect.Observation
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observation) != nil || observation.Validate() != nil ||
		observation.State != approvaleffect.StateAbsent || observation.ApprovalRequestID != requestID || reader.calls != 1 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("closed absent observation failed: status=%d calls=%d body=%s", response.Code, reader.calls, response.Body.String())
	}
	reader.observed = true
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, request)
	if json.Unmarshal(response.Body.Bytes(), &observation) != nil || observation.State != approvaleffect.StateObserved {
		t.Fatalf("observed effect was not conservative: %s", response.Body.String())
	}
	invalid := httptest.NewRequest(http.MethodGet, "/?command_type=TOKEN_CREDENTIAL_REVOKE&command_fingerprint="+fingerprint, nil).WithContext(ctx)
	invalid.SetPathValue("approval_request_id", requestID.String())
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, invalid)
	if response.Code != http.StatusBadRequest || reader.calls != 2 {
		t.Fatal("cross-domain command reached Billing authority")
	}
	other, _ := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	denied := request.Clone(other)
	denied.SetPathValue("approval_request_id", requestID.String())
	response = httptest.NewRecorder()
	service.approvalEffectObservation(response, denied)
	if response.Code != http.StatusForbidden || reader.calls != 2 {
		t.Fatal("non-Account workload reached Billing effect authority")
	}
}
