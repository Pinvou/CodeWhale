package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type recoveryZeroEffectRepositoryStub struct {
	Repository
	calls int
	err   error
}

const zeroEffectTarget = "/internal/v1/approval-recovery/zero-effect"

func (stub *recoveryZeroEffectRepositoryStub) InvalidateOriginalApprovalZeroEffect(_ context.Context, _ ApprovalRecoveryCandidate, _ RecoveryQueryLease) error {
	stub.calls++
	return stub.err
}

// recoveryReadOnlyRepositoryStub deliberately does not implement the confirmer,
// so an unwired repository must fail closed instead of degrading.
type recoveryReadOnlyRepositoryStub struct {
	Repository
}

type recoveryZeroEffectResultStub struct {
	calls int
	state string
	err   error
}

func (stub *recoveryZeroEffectResultStub) ObserveRecoveryResult(_ context.Context, _ ApprovalRecoveryCandidate) (RecoveryResultObservation, error) {
	stub.calls++
	if stub.err != nil {
		return RecoveryResultObservation{}, stub.err
	}
	return RecoveryResultObservation{State: stub.state, ObservedAt: time.Now().UTC()}, nil
}

type recoveryZeroEffectEffectStub struct {
	calls   int
	state   string
	foreign bool
	err     error
}

func (stub *recoveryZeroEffectEffectStub) Observe(_ context.Context, requestID uuid.UUID, command, fingerprint string) (approvaleffect.Observation, error) {
	stub.calls++
	if stub.err != nil {
		return approvaleffect.Observation{}, stub.err
	}
	if stub.foreign {
		// An observation for another request is never this execution's effect.
		requestID = uuid.New()
	}
	return approvaleffect.Observation{
		SchemaVersion: approvaleffect.SchemaVersion, Authority: approvaleffect.AuthorityBilling,
		ApprovalRequestID: requestID, CommandType: command, CommandFingerprint: fingerprint,
		State: stub.state, ObservedAt: time.Now().UTC(),
	}, nil
}

// The zero-effect invalidation is the only terminal transition the recovery
// worker may reach, so its surface must fail closed on every unproven input
// before the Account CAS - and must keep failing closed when a worker tries to
// supply its own verdict or when either owner authority is unavailable.
func TestRecoveryZeroEffectEndpointFailsClosed(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	candidate := ApprovalRecoveryCandidate{
		RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a100000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, RequestStatus: "EXECUTION_FAILED", ExecutionStatus: "FAILED",
		ExecutionDeadline: time.Now().UTC().Add(-time.Minute), WindowElapsed: true,
		Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, ""),
	}
	lease := RecoveryQueryLease{RequestID: candidate.RequestID, Token: uuid.New(), Until: time.Now().UTC().Add(15 * time.Second), QueryCount: 1, Version: 1}
	valid, err := json.Marshal(approvalRecoveryConfirmation{Candidate: candidate, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	foreignLease, err := json.Marshal(approvalRecoveryConfirmation{Candidate: candidate,
		Lease: RecoveryQueryLease{RequestID: uuid.New(), Token: uuid.New(), Version: 1}})
	if err != nil {
		t.Fatal(err)
	}
	unknownCandidate := candidate
	unknownCandidate.Durable = "billing-unknown-executor-p1-v1"
	unknownBody, err := json.Marshal(approvalRecoveryConfirmation{Candidate: unknownCandidate, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, service, body, contentType, wantCode string
		readErr, effectErr, writeErr               error
		state, effectState                         string
		foreignEffect                              bool
		wantStatus, wantReads, wantEffectReads     int
		wantWrites                                 int
	}{
		{"proven zero effect", "account-approval-recovery", string(valid), "application/json", "",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusNoContent, 1, 1, 1},
		{"other identity", "billing-service", string(valid), "application/json", "",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusForbidden, 0, 0, 0},
		{"positive result wins", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_UNCONFIRMED",
			nil, nil, nil, "SUCCEEDED", approvaleffect.StateAbsent, false, http.StatusConflict, 1, 0, 0},
		{"result authority unavailable", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_UNAVAILABLE",
			errors.New("down"), nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusServiceUnavailable, 1, 0, 0},
		{"effect observed", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_EFFECT_OBSERVED",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateObserved, false, http.StatusConflict, 1, 1, 0},
		{"effect for another request", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_EFFECT_OBSERVED",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, true, http.StatusConflict, 1, 1, 0},
		{"effect authority unavailable", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_UNAVAILABLE",
			nil, errors.New("down"), nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusServiceUnavailable, 1, 1, 0},
		{"stale binding", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_CONFLICT",
			nil, nil, ErrApprovalRecoveryZeroEffectConflict, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusConflict, 1, 1, 1},
		{"repository unavailable", "account-approval-recovery", string(valid), "application/json", "APPROVAL_RECOVERY_UNAVAILABLE",
			nil, nil, errors.New("down"), "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusServiceUnavailable, 1, 1, 1},
		{"worker verdict injection", "account-approval-recovery", strings.Replace(string(valid), "{", `{"verdict":"ZERO_EFFECT",`, 1), "application/json", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
		{"unknown binding", "account-approval-recovery", string(unknownBody), "application/json", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
		{"foreign lease", "account-approval-recovery", string(foreignLease), "application/json", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
		{"wrong media type", "account-approval-recovery", string(valid), "text/plain", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
		{"oversized body", "account-approval-recovery", strings.Repeat("a", 17<<10), "application/json", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
		{"duplicate keys", "account-approval-recovery", strings.Replace(string(valid), "{", `{"candidate":null,`, 1), "application/json", "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			nil, nil, nil, "UNCONFIRMED", approvaleffect.StateAbsent, false, http.StatusBadRequest, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.service, "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, zeroEffectTarget, bytes.NewBufferString(tc.body)).WithContext(ctx)
			request.Header.Set("Content-Type", tc.contentType)
			response := httptest.NewRecorder()
			repository := &recoveryZeroEffectRepositoryStub{err: tc.writeErr}
			results := &recoveryZeroEffectResultStub{state: tc.state, err: tc.readErr}
			effects := &recoveryZeroEffectEffectStub{state: tc.effectState, foreign: tc.foreignEffect, err: tc.effectErr}
			NewService(repository, nil).WithApprovalRecoveryResults(results).WithApprovalRecoveryEffects(effects).
				internalInvalidateApprovalRecoveryZeroEffect(response, request)
			if response.Code != tc.wantStatus || response.Header().Get("Cache-Control") != "no-store" ||
				results.calls != tc.wantReads || effects.calls != tc.wantEffectReads || repository.calls != tc.wantWrites {
				t.Fatalf("status=%d reads=%d effect_reads=%d writes=%d body=%s",
					response.Code, results.calls, effects.calls, repository.calls, response.Body.String())
			}
			if tc.wantCode != "" && !strings.Contains(response.Body.String(), tc.wantCode) {
				t.Fatalf("expected %s in body: %s", tc.wantCode, response.Body.String())
			}
		})
	}
}

// A missing, malformed or partially wired surface must stay unavailable rather
// than fall back to a weaker check.
func TestRecoveryZeroEffectEndpointRequiresEveryAuthority(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	candidate := ApprovalRecoveryCandidate{
		RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a100000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, RequestStatus: "EXECUTION_FAILED", ExecutionStatus: "FAILED",
		ExecutionDeadline: time.Now().UTC().Add(-time.Minute), WindowElapsed: true,
		Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "DOMAIN_RESOURCE_NOT_FOUND"),
	}
	lease := RecoveryQueryLease{RequestID: candidate.RequestID, Token: uuid.New(), Version: 1}
	body, err := json.Marshal(approvalRecoveryConfirmation{Candidate: candidate, Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	unwired := []struct {
		name, target, wantCode string
		wantStatus             int
		service                *Service
	}{
		{name: "no result authority", target: zeroEffectTarget, wantStatus: http.StatusServiceUnavailable, wantCode: "APPROVAL_RECOVERY_UNAVAILABLE",
			service: NewService(&recoveryZeroEffectRepositoryStub{}, nil).WithApprovalRecoveryEffects(&recoveryZeroEffectEffectStub{state: approvaleffect.StateAbsent})},
		{name: "no effect authority", target: zeroEffectTarget, wantStatus: http.StatusServiceUnavailable, wantCode: "APPROVAL_RECOVERY_UNAVAILABLE",
			service: NewService(&recoveryZeroEffectRepositoryStub{}, nil).WithApprovalRecoveryResults(&recoveryZeroEffectResultStub{state: "UNCONFIRMED"})},
		{name: "read-only repository", target: zeroEffectTarget, wantStatus: http.StatusServiceUnavailable, wantCode: "APPROVAL_RECOVERY_UNAVAILABLE",
			service: NewService(&recoveryReadOnlyRepositoryStub{}, nil).WithApprovalRecoveryResults(&recoveryZeroEffectResultStub{state: "UNCONFIRMED"}).
				WithApprovalRecoveryEffects(&recoveryZeroEffectEffectStub{state: approvaleffect.StateAbsent})},
		{name: "query string", target: zeroEffectTarget + "?apply=true", wantStatus: http.StatusBadRequest, wantCode: "INVALID_APPROVAL_RECOVERY_INVALIDATION",
			service: NewService(&recoveryZeroEffectRepositoryStub{}, nil).WithApprovalRecoveryResults(&recoveryZeroEffectResultStub{state: "UNCONFIRMED"}).
				WithApprovalRecoveryEffects(&recoveryZeroEffectEffectStub{state: approvaleffect.StateAbsent})},
	}
	for _, tc := range unwired {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), "account-approval-recovery", "APPROVAL_RECOVERY_QUERY", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, tc.target, bytes.NewReader(body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			tc.service.internalInvalidateApprovalRecoveryZeroEffect(response, request)
			if response.Code != tc.wantStatus || !strings.Contains(response.Body.String(), tc.wantCode) {
				t.Fatalf("status=%d want=%d expected %s in body: %s", response.Code, tc.wantStatus, tc.wantCode, response.Body.String())
			}
		})
	}
}
