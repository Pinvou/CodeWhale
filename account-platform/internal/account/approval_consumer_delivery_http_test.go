package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type approvalConsumerDeliveryRepositoryStub struct {
	Repository
	resolved     ApprovalAuthorizationBinding
	observation  ApprovalDeliveryObservation
	beginBinding ApprovalAuthorizationBinding
	state        ApprovalDeliveryState
}

func (stub *approvalConsumerDeliveryRepositoryStub) BeginApprovalExecution(_ context.Context, requestID uuid.UUID, executorIdentity, _ string, _, _ json.RawMessage) (ApprovalExecution, error) {
	return ApprovalExecution{ApprovalRequestID: requestID, ExecutorIdentity: executorIdentity, Status: "EXECUTING"}, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) CompleteApprovalExecution(context.Context, uuid.UUID, string, string, string, json.RawMessage, json.RawMessage, string) error {
	return nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) BeginApprovalExecutionWithAuthorization(_ context.Context, requestID uuid.UUID, executorIdentity, _ string, _, _ json.RawMessage, binding ApprovalAuthorizationBinding) (ApprovalExecution, error) {
	stub.beginBinding = binding
	return ApprovalExecution{ApprovalRequestID: requestID, ExecutorIdentity: executorIdentity, Status: "EXECUTING"}, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) ResolveApprovalAuthorizationBinding(_ context.Context, eventID uuid.UUID, durable, workload string) (ApprovalAuthorizationBinding, error) {
	stub.resolved.EventID, stub.resolved.Durable, stub.resolved.Workload = eventID, durable, workload
	return stub.resolved, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) ObserveApprovalAuthorizationDelivery(_ context.Context, observation ApprovalDeliveryObservation) (ApprovalDeliveryState, error) {
	stub.observation = observation
	return stub.state, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) GrantApprovalAuthorizationDomainAttempt(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error) {
	return stub.state, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) RecordApprovalAuthorizationDomainFailure(context.Context, uuid.UUID, string, string, int64, string) (ApprovalDeliveryState, error) {
	return stub.state, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) RecordApprovalAuthorizationResult(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error) {
	return stub.state, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) RecordApprovalAuthorizationDLQPubAck(context.Context, uuid.UUID, string, string, int64, string, approvalconsumer.Sequence) (ApprovalDeliveryState, error) {
	return stub.state, nil
}

func (stub *approvalConsumerDeliveryRepositoryStub) RecordApprovalAuthorizationTerminal(context.Context, uuid.UUID, string, string, int64) (ApprovalDeliveryState, error) {
	return stub.state, nil
}

func TestApprovalConsumerDeliveryHTTPUsesTrustedPathAndWorkloadIdentity(t *testing.T) {
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	requestID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	durable := "billing-wallet-adjustment-executor-p1-v1"
	digest := strings.Repeat("a", 64)
	stub := &approvalConsumerDeliveryRepositoryStub{state: ApprovalDeliveryState{State: "READY", Version: 1}}
	service := NewService(stub, nil)
	ctx, err := serviceauth.ContextForJob(context.Background(), "billing-command-executor", "APPROVAL_AUTHORIZATION_CONSUME", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"approval_request_id": requestID, "approved_request_version": 3, "approval_epoch": 1,
		"serialization_version": "LEGACY_TYPED_V1", "provenance": "FROZEN_AT_APPROVAL",
		"event_digest": digest, "digest_kind": "DELIVERY_BYTES", "command_type": "WALLET_ADJUSTMENT",
		"policy_version": 1, "executor_identity": "billing-command-executor", "stream": "ACCOUNT_PLATFORM", "stream_sequence": "9",
	})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-authorization/events/"+eventID.String()+"/deliveries/"+durable+"/observe", strings.NewReader(string(body))).WithContext(ctx)
	request.SetPathValue("event_id", eventID.String())
	request.SetPathValue("durable", durable)
	response := httptest.NewRecorder()
	service.observeApprovalAuthorizationDelivery(response, request)
	if response.Code != http.StatusOK || stub.observation.Binding.EventID != eventID || stub.observation.Binding.Durable != durable ||
		stub.observation.Binding.Workload != "billing-command-executor" || stub.observation.Binding.ApprovalRequestID != requestID ||
		stub.observation.Stream != "ACCOUNT_PLATFORM" || stub.observation.StreamSequence != 9 {
		t.Fatalf("status=%d observation=%+v body=%s", response.Code, stub.observation, response.Body.String())
	}
}

func TestApprovalConsumerDeliveryHTTPRejectsCallerChosenIdentityFields(t *testing.T) {
	stub := &approvalConsumerDeliveryRepositoryStub{}
	service := NewService(stub, nil)
	ctx, err := serviceauth.ContextForJob(context.Background(), "billing-command-executor", "APPROVAL_AUTHORIZATION_CONSUME", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"binding":{"event_id":"`+uuid.NewString()+`"},"stream":"ACCOUNT_PLATFORM","stream_sequence":"1"}`)).WithContext(ctx)
	request.SetPathValue("event_id", eventID.String())
	request.SetPathValue("durable", "billing-wallet-adjustment-executor-p1-v1")
	response := httptest.NewRecorder()
	service.observeApprovalAuthorizationDelivery(response, request)
	if response.Code != http.StatusBadRequest || stub.observation.Binding.EventID != uuid.Nil {
		t.Fatalf("caller identity override status=%d observation=%+v body=%s", response.Code, stub.observation, response.Body.String())
	}
}

func TestApprovalConsumerDeliveryHTTPRejectsNoncanonicalSequence(t *testing.T) {
	eventID := uuid.New()
	requestID := uuid.New()
	digest := strings.Repeat("a", 64)
	ctx, err := serviceauth.ContextForJob(context.Background(), "billing-command-executor", "APPROVAL_AUTHORIZATION_CONSUME", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []string{`0`, `"01"`, `"+1"`, `" 1"`, `"18446744073709551616"`, `1`} {
		t.Run(sequence, func(t *testing.T) {
			stub := &approvalConsumerDeliveryRepositoryStub{}
			body := `{"approval_request_id":"` + requestID.String() + `","approved_request_version":3,"approval_epoch":1,` +
				`"serialization_version":"LEGACY_TYPED_V1","provenance":"FROZEN_AT_APPROVAL",` +
				`"event_digest":"` + digest + `","digest_kind":"DELIVERY_BYTES","command_type":"WALLET_ADJUSTMENT",` +
				`"policy_version":1,"executor_identity":"billing-command-executor","stream":"ACCOUNT_PLATFORM","stream_sequence":` + sequence + `}`
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
			request.SetPathValue("event_id", eventID.String())
			request.SetPathValue("durable", "billing-wallet-adjustment-executor-p1-v1")
			response := httptest.NewRecorder()
			NewService(stub, nil).observeApprovalAuthorizationDelivery(response, request)
			if response.Code != http.StatusBadRequest || stub.observation.StreamSequence != 0 {
				t.Fatalf("sequence=%s status=%d observation=%+v", sequence, response.Code, stub.observation)
			}
		})
	}
}

func TestApprovalConsumerDeliveryHTTPRequiresTrustedWorkload(t *testing.T) {
	stub := &approvalConsumerDeliveryRepositoryStub{}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetPathValue("event_id", uuid.NewString())
	request.SetPathValue("durable", "billing-wallet-adjustment-executor-p1-v1")
	response := httptest.NewRecorder()
	NewService(stub, nil).resolveApprovalAuthorizationBinding(response, request)
	if response.Code != http.StatusForbidden || stub.resolved.EventID != uuid.Nil {
		t.Fatalf("untrusted workload status=%d resolved=%+v", response.Code, stub.resolved)
	}
}

func TestApprovalExecutionHTTPForwardsClosedAuthorizationBinding(t *testing.T) {
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	requestID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	binding := ApprovalAuthorizationBinding{
		EventID: eventID, Durable: "billing-wallet-adjustment-executor-p1-v1", Workload: "billing-command-executor",
		ApprovalRequestID: requestID, ApprovedRequestVersion: 3, ApprovalEpoch: 1,
		SerializationVersion: "LEGACY_TYPED_V1", Provenance: "FROZEN_AT_APPROVAL", EventDigest: strings.Repeat("a", 64),
		DigestKind: "DELIVERY_BYTES", CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, ExecutorIdentity: "billing-command-executor",
	}
	body, _ := json.Marshal(map[string]any{
		"job_run_id": "job-1", "observed_versions": map[string]any{}, "observed_financial_snapshot": map[string]any{},
		"authorization_binding": binding,
	})
	ctx, err := serviceauth.ContextForJob(context.Background(), "billing-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))).WithContext(ctx)
	request.SetPathValue("approval_request_id", requestID.String())
	response := httptest.NewRecorder()
	stub := &approvalConsumerDeliveryRepositoryStub{}
	NewService(stub, nil).beginApprovalExecution(response, request)
	if response.Code != http.StatusOK || stub.beginBinding != binding {
		t.Fatalf("status=%d binding=%+v body=%s", response.Code, stub.beginBinding, response.Body.String())
	}
}

func TestApprovalExecutorIdentityUsesClosedWorkloadMapping(t *testing.T) {
	for _, item := range []struct {
		service string
		want    string
	}{
		{service: "billing-service", want: "billing-command-executor"},
		{service: "token-command-executor", want: "token-command-executor"},
		{service: "unrelated-service", want: "unrelated-service"},
	} {
		ctx, err := serviceauth.ContextForJob(context.Background(), item.service, "APPROVAL_EXECUTION", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
		identity, ok := approvalExecutorFromContext(httptest.NewRecorder(), request)
		if !ok || identity != item.want {
			t.Fatalf("service=%s identity=%s ok=%t", item.service, identity, ok)
		}
	}
}
