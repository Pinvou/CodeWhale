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
	state string
	calls int
}

func (stub *billingRecoveryResultStub) ObserveBillingApprovalResult(_ context.Context, _, _ uuid.UUID, _ int64, _ string) (string, time.Time, error) {
	stub.calls++
	return stub.state, time.Now().UTC(), nil
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
		{"account service refused", "account-service", base, http.StatusForbidden, 1},
		{"duplicate epoch", "account-approval-recovery", base + "&approval_epoch=2", http.StatusBadRequest, 1},
		{"epoch alias", "account-approval-recovery", strings.Replace(base, "approval_epoch=2", "approval_epoch=02", 1), http.StatusBadRequest, 1},
		{"cross domain", "account-approval-recovery", strings.Replace(base, "PAYMENT_PROVIDER_RESUME", "TOKEN_CREDENTIAL_REVOKE", 1), http.StatusBadRequest, 1},
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
}
