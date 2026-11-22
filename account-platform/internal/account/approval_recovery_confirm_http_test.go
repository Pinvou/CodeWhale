package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type recoveryConfirmRepositoryStub struct {
	Repository
	calls int
	err   error
}

func (stub *recoveryConfirmRepositoryStub) ConfirmOriginalApprovalSuccess(_ context.Context, _ ApprovalRecoveryCandidate, _ RecoveryQueryLease, _ RecoveryResultObservation) error {
	stub.calls++
	return stub.err
}

type recoveryConfirmResultStub struct {
	calls int
	state string
	err   error
}

func (stub *recoveryConfirmResultStub) ObserveRecoveryResult(_ context.Context, _ ApprovalRecoveryCandidate) (RecoveryResultObservation, error) {
	stub.calls++
	if stub.err != nil {
		return RecoveryResultObservation{}, stub.err
	}
	return RecoveryResultObservation{State: stub.state, ObservedAt: time.Now().UTC()}, nil
}

func TestRecoveryConfirmationRechecksUnderDedicatedIdentity(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	candidate := ApprovalRecoveryCandidate{
		RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a100000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, RequestStatus: "EXECUTION_FAILED", ExecutionStatus: "FAILED",
		ExecutionDeadline: time.Now().UTC().Add(-time.Minute),
		Investigation:     approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, ""),
	}
	lease := RecoveryQueryLease{RequestID: candidate.RequestID, Token: uuid.New(), Until: time.Now().UTC().Add(15 * time.Second), QueryCount: 1, Version: 1}
	valid, err := json.Marshal(approvalRecoveryConfirmation{Candidate: candidate, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, service, body, contentType, state string
		readErr, confirmErr                     error
		wantStatus, wantReads, wantWrites       int
	}{
		{"success", "account-approval-recovery", string(valid), "application/json", "SUCCEEDED", nil, nil, http.StatusNoContent, 1, 1},
		{"other identity", "billing-service", string(valid), "application/json", "SUCCEEDED", nil, nil, http.StatusForbidden, 0, 0},
		{"unconfirmed", "account-approval-recovery", string(valid), "application/json", "UNCONFIRMED", nil, nil, http.StatusConflict, 1, 0},
		{"domain unavailable", "account-approval-recovery", string(valid), "application/json", "", errors.New("down"), nil, http.StatusServiceUnavailable, 1, 0},
		{"stale binding", "account-approval-recovery", string(valid), "application/json", "SUCCEEDED", nil, ErrApprovalRecoveryConfirmConflict, http.StatusConflict, 1, 1},
		{"worker result injection", "account-approval-recovery", string(valid[:len(valid)-1]) + `,"result":{"state":"SUCCEEDED"}}`, "application/json", "SUCCEEDED", nil, nil, http.StatusBadRequest, 0, 0},
		{"wrong media type", "account-approval-recovery", string(valid), "text/plain", "SUCCEEDED", nil, nil, http.StatusBadRequest, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.service, "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-recovery/confirm", bytes.NewBufferString(tc.body)).WithContext(ctx)
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			repo := &recoveryConfirmRepositoryStub{err: tc.confirmErr}
			reader := &recoveryConfirmResultStub{state: tc.state, err: tc.readErr}
			NewService(repo, nil).WithApprovalRecoveryResults(reader).internalConfirmApprovalRecovery(response, request)
			if response.Code != tc.wantStatus || response.Header().Get("Cache-Control") != "no-store" ||
				reader.calls != tc.wantReads || repo.calls != tc.wantWrites {
				t.Fatalf("status=%d reads=%d writes=%d body=%s", response.Code, reader.calls, repo.calls, response.Body.String())
			}
		})
	}
}
