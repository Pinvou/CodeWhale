package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type recoveryPlanHTTPRepositoryStub struct {
	Repository
	plan  ApprovalRecoveryPlan
	calls int
}

func (stub *recoveryPlanHTTPRepositoryStub) PlanProofApprovalRecovery(_ context.Context, limit int) (ApprovalRecoveryPlan, error) {
	stub.calls++
	if limit != 2 {
		return ApprovalRecoveryPlan{}, ErrApprovalRecoveryPlanInvalid
	}
	return stub.plan, nil
}

func TestInternalApprovalRecoveryPlanRequiresDedicatedIdentityAndClosedLimit(t *testing.T) {
	requestID := uuid.New()
	stub := &recoveryPlanHTTPRepositoryStub{plan: ApprovalRecoveryPlan{
		ObservedAt: time.Now().UTC(), Control: validAdminApprovalControl(AdminApprovalModeLegacy, AdminApprovalRelayRunning),
		Candidates: []ApprovalRecoveryCandidate{{RequestID: requestID, OriginalKey: "original-only"}},
	}}
	for _, tc := range []struct {
		name, service, rawURL string
		wantStatus            int
		wantCalls             int
	}{
		{"dedicated recovery", "account-approval-recovery", "/internal/v1/approval-recovery/plan?limit=2", http.StatusOK, 1},
		{"account service refused", "account-service", "/internal/v1/approval-recovery/plan?limit=2", http.StatusForbidden, 1},
		{"other workload refused", "billing-service", "/internal/v1/approval-recovery/plan?limit=2", http.StatusForbidden, 1},
		{"duplicate limit refused", "account-approval-recovery", "/internal/v1/approval-recovery/plan?limit=2&limit=2", http.StatusBadRequest, 1},
		{"alias refused", "account-approval-recovery", "/internal/v1/approval-recovery/plan?limit=02", http.StatusBadRequest, 1},
		{"extra filter refused", "account-approval-recovery", "/internal/v1/approval-recovery/plan?limit=2&status=FAILED", http.StatusBadRequest, 1},
		{"oversized refused", "account-approval-recovery", "/internal/v1/approval-recovery/plan?limit=201", http.StatusBadRequest, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.service, "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, tc.rawURL, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			NewService(stub, nil).internalApprovalRecoveryPlan(response, request)
			if response.Code != tc.wantStatus || response.Header().Get("Cache-Control") != "no-store" || stub.calls != tc.wantCalls {
				t.Fatalf("status=%d calls=%d headers=%v body=%s", response.Code, stub.calls, response.Header(), response.Body.String())
			}
			if response.Code == http.StatusOK {
				var plan approvalRecoveryPlanView
				if json.Unmarshal(response.Body.Bytes(), &plan) != nil || plan.SchemaVersion != 1 || len(plan.Candidates) != 1 || plan.Candidates[0].RequestID != requestID {
					t.Fatalf("incorrect bounded plan: %s", response.Body.String())
				}
			}
		})
	}
}
