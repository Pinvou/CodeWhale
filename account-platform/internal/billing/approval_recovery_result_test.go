package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type billingRecoveryResultStub struct {
	Repository
	state            string
	domainResult     json.RawMessage
	ledgerReferences json.RawMessage
	calls            int
}

func (stub *billingRecoveryResultStub) ObserveBillingApprovalResult(_ context.Context, _, _ uuid.UUID, _ int64, _ string) (billingRecoveryResultObservation, error) {
	stub.calls++
	return billingRecoveryResultObservation{State: stub.state, DomainResult: stub.domainResult,
		LedgerReferences: stub.ledgerReferences, ObservedAt: time.Now().UTC()}, nil
}

func TestBillingRecoveryResultRequiresDedicatedIdentityAndFrozenEvent(t *testing.T) {
	requestID, eventID := uuid.New(), uuid.New()
	stub := &billingRecoveryResultStub{state: "UNCONFIRMED"}
	service := &Service{repository: stub}
	base := "/internal/v1/approval-recovery/results/" + requestID.String() + "?command_type=PAYMENT_PROVIDER_RESUME&original_event_id=" + eventID.String() + "&approval_epoch=2"
	for _, tc := range []struct {
		name, identity, rawURL string
		status, calls          int
	}{
		{"dedicated", "account-approval-recovery", base, http.StatusOK, 1},
		{"independent Account recheck", "account-service", base, http.StatusOK, 2},
		{"executor refused", "billing-command-executor", base, http.StatusForbidden, 2},
		{"duplicate epoch", "account-approval-recovery", base + "&approval_epoch=2", http.StatusBadRequest, 2},
		{"epoch alias", "account-approval-recovery", strings.Replace(base, "approval_epoch=2", "approval_epoch=02", 1), http.StatusBadRequest, 2},
		{"cross domain", "account-approval-recovery", strings.Replace(base, "PAYMENT_PROVIDER_RESUME", "TOKEN_CREDENTIAL_REVOKE", 1), http.StatusBadRequest, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.identity, "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.rawURL, nil).WithContext(ctx)
			request.SetPathValue("approval_request_id", requestID.String())
			response := httptest.NewRecorder()
			service.billingApprovalRecoveryResult(response, request)
			if response.Code != tc.status || stub.calls != tc.calls || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, stub.calls, response.Body.String())
			}
			if response.Code == http.StatusOK {
				var result billingRecoveryResult
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.State != "UNCONFIRMED" || result.OriginalEventID != eventID {
					t.Fatalf("invalid result: %s", response.Body.String())
				}
			}
		})
	}
	stub.state = "SUCCEEDED"
	stub.domainResult = json.RawMessage(`{"execution_id":"` + uuid.NewString() + `","provider_id":"provider-a","status":"RESUMED"}`)
	stub.ledgerReferences = json.RawMessage(`{}`)
	ctx, _ := serviceauth.ContextForJob(context.Background(), "account-approval-recovery", "APPROVAL_RECOVERY_QUERY", uuid.NewString())
	request := httptest.NewRequest(http.MethodGet, base, nil).WithContext(ctx)
	request.SetPathValue("approval_request_id", requestID.String())
	response := httptest.NewRecorder()
	service.billingApprovalRecoveryResult(response, request)
	var confirmed billingRecoveryResult
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &confirmed) != nil ||
		confirmed.State != "SUCCEEDED" || len(confirmed.DomainResult) == 0 || string(confirmed.LedgerReferences) != "{}" {
		t.Fatalf("positive domain result missing: %s", response.Body.String())
	}
}
