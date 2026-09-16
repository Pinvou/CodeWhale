package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type authorizationAccountStub struct {
	binding                                      ApprovalAuthorizationBinding
	route                                        *ApprovalAuthorizationRoute
	state                                        ApprovalAuthorizationDeliveryState
	read                                         ApprovalAuthorizationDeliveryState
	command                                      ApprovedCommand
	order                                        []string
	begin, grant, failure, result, dlq, terminal int
	errAt, terminalState                         string
}

func (s *authorizationAccountStub) ResolveAuthorizationRoute(context.Context, uuid.UUID) (ApprovalAuthorizationRoute, error) {
	if s.route != nil {
		return *s.route, s.fail("route")
	}
	binding, err := s.binding, s.fail("route")
	if err != nil {
		return ApprovalAuthorizationRoute{}, err
	}
	return ApprovalAuthorizationRoute{EventID: binding.EventID, ApprovalRequestID: binding.ApprovalRequestID,
		ApprovedRequestVersion: binding.ApprovedRequestVersion, ApprovalEpoch: binding.ApprovalEpoch,
		SerializationVersion: binding.SerializationVersion, Provenance: binding.Provenance, EventDigest: binding.EventDigest,
		DigestKind: binding.DigestKind, CommandType: binding.CommandType, PolicyVersion: binding.PolicyVersion,
		ExecutorIdentity: binding.ExecutorIdentity, CanonicalWorkload: binding.Workload, CanonicalDurable: binding.Durable}, nil
}

func TestAuthorizationCommandConsumerAcknowledgesAuthenticatedNonTargetWithoutStateOrDomain(t *testing.T) {
	consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", 0)
	consumer.durable = "billing-wallet-adjustment-executor-p1-v1"
	binding := account.binding
	account.route = &ApprovalAuthorizationRoute{EventID: binding.EventID, ApprovalRequestID: binding.ApprovalRequestID,
		ApprovedRequestVersion: binding.ApprovedRequestVersion, ApprovalEpoch: binding.ApprovalEpoch,
		SerializationVersion: binding.SerializationVersion, Provenance: binding.Provenance, EventDigest: binding.EventDigest,
		DigestKind: binding.DigestKind, CommandType: binding.CommandType, PolicyVersion: binding.PolicyVersion,
		ExecutorIdentity: binding.ExecutorIdentity, CanonicalWorkload: binding.Workload, CanonicalDurable: binding.Durable}
	consumer.Handle(context.Background(), delivery)
	if delivery.ack != 1 || delivery.nak != 0 || delivery.publish != 0 || domain.calls != 0 || account.begin != 0 ||
		account.grant != 0 || !equalStrings(account.order, []string{"ack"}) {
		t.Fatalf("non-target outcome ack=%d nak=%d publish=%d domain=%d begin=%d grant=%d order=%v",
			delivery.ack, delivery.nak, delivery.publish, domain.calls, account.begin, account.grant, account.order)
	}
}

func (s *authorizationAccountStub) fail(name string) error {
	if s.errAt == name {
		return errors.New("simulated")
	}
	return nil
}
func (s *authorizationAccountStub) ResolveAuthorizationBinding(context.Context, uuid.UUID, string) (ApprovalAuthorizationBinding, error) {
	s.order = append(s.order, "resolve")
	return s.binding, s.fail("resolve")
}
func (s *authorizationAccountStub) ObserveAuthorizationDelivery(context.Context, ApprovalAuthorizationBinding, string, approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error) {
	s.order = append(s.order, "observe")
	return s.state, s.fail("observe")
}
func (s *authorizationAccountStub) AuthorizationDeliveryState(context.Context, ApprovalAuthorizationBinding) (ApprovalAuthorizationDeliveryState, error) {
	s.order = append(s.order, "read")
	return s.read, s.fail("read")
}
func (s *authorizationAccountStub) BeginAuthorized(context.Context, ApprovalAuthorizationBinding, string, json.RawMessage, json.RawMessage) (ApprovedCommand, error) {
	s.begin++
	s.order = append(s.order, "begin")
	return s.command, s.fail("begin")
}
func (s *authorizationAccountStub) Complete(_ context.Context, _ uuid.UUID, _ string, result string, _ json.RawMessage, _ json.RawMessage, _ string) error {
	s.order = append(s.order, "complete:"+result)
	return s.fail("complete")
}
func (s *authorizationAccountStub) GrantAuthorizationAttempt(context.Context, ApprovalAuthorizationBinding, int64) (ApprovalAuthorizationDeliveryState, error) {
	s.grant++
	s.order = append(s.order, "grant")
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: s.state.DomainAttemptCount + 1, State: "DOMAIN_INFLIGHT", Version: s.state.Version + 1}, s.fail("grant")
}
func (s *authorizationAccountStub) RecordAuthorizationFailure(context.Context, ApprovalAuthorizationBinding, int64, string) (ApprovalAuthorizationDeliveryState, error) {
	s.failure++
	s.order = append(s.order, "failure")
	attempts := s.state.DomainAttemptCount + 1
	state := "READY"
	if attempts == approvalconsumer.MaxDomainAttempts {
		state = "DOMAIN_INFLIGHT"
	}
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: attempts, State: state, Version: s.state.Version + 2}, s.fail("failure")
}
func (s *authorizationAccountStub) RecordAuthorizationResult(_ context.Context, _ ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	s.result++
	s.order = append(s.order, "result")
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: s.state.DomainAttemptCount, State: "RESULT_CONFIRMED", Version: version + 1}, s.fail("result")
}
func (s *authorizationAccountStub) RecordAuthorizationDLQ(_ context.Context, _ ApprovalAuthorizationBinding, version int64, _ string, _ approvalconsumer.Sequence) (ApprovalAuthorizationDeliveryState, error) {
	s.dlq++
	s.order = append(s.order, "dlq")
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: 12, State: "DLQ_PUBLISHED_ACK_PENDING", Version: version + 1}, s.fail("dlq")
}
func (s *authorizationAccountStub) RecordAuthorizationTerminal(_ context.Context, _ ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	s.terminal++
	s.order = append(s.order, "terminal")
	state := s.terminalState
	if state == "" {
		state = "TERMINAL_CONFIRMED"
	}
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: s.state.DomainAttemptCount, State: state, Version: version + 1}, s.fail("terminal")
}

type authorizationDomainStub struct {
	result    authorizationDomainResult
	found     bool
	lookupErr error
	attempt   authorizationDomainAttempt
	calls     int
}

func (s *authorizationDomainStub) LookupAuthorizationResult(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent) (authorizationDomainResult, bool, error) {
	return s.result, s.found, s.lookupErr
}
func (s *authorizationDomainStub) ExecuteAuthorizationAttempt(context.Context, ApprovalAuthorizationBinding, approvalencoding.AuthorizationEvent, ApprovedCommand) authorizationDomainAttempt {
	s.calls++
	return s.attempt
}

type authorizationDeliveryStub struct {
	raw               []byte
	id                string
	meta              events.AuthorizationDeliveryMetadata
	order             *[]string
	ack, nak, publish int
	errAt             string
}

func (s *authorizationDeliveryStub) DeliveryBytes() []byte                          { return append([]byte(nil), s.raw...) }
func (s *authorizationDeliveryStub) MessageID() string                              { return s.id }
func (s *authorizationDeliveryStub) Metadata() events.AuthorizationDeliveryMetadata { return s.meta }
func (s *authorizationDeliveryStub) DoubleAck(context.Context) error {
	s.ack++
	*s.order = append(*s.order, "ack")
	if s.errAt == "ack" {
		return errors.New("simulated")
	}
	return nil
}
func (s *authorizationDeliveryStub) NakWithDelay(time.Duration) error {
	s.nak++
	*s.order = append(*s.order, "nak")
	if s.errAt == "nak" {
		return errors.New("simulated")
	}
	return nil
}
func (s *authorizationDeliveryStub) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	s.publish++
	*s.order = append(*s.order, "publish-dlq")
	if s.errAt == "publish" {
		return events.AuthorizationDLQPubAck{}, errors.New("simulated")
	}
	return events.AuthorizationDLQPubAck{Stream: "ACCOUNT_PLATFORM", StreamSequence: 91}, nil
}

func TestAuthorizationCommandConsumerStateMachine(t *testing.T) {
	for _, test := range []struct {
		name, state                               string
		attempts                                  int16
		read                                      *string
		resultFound                               bool
		attempt                                   authorizationDomainAttempt
		wantOrder                                 []string
		wantAck, wantNak, wantPublish, wantDomain int
	}{
		{"ready-success", "READY", 0, nil, false, authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: json.RawMessage(`{"ok":true}`)}}, []string{"resolve", "observe", "begin", "grant", "complete:EXECUTED", "result", "ack", "terminal"}, 1, 0, 0, 1},
		{"inflight-unknown", "DOMAIN_INFLIGHT", 1, nil, false, authorizationDomainAttempt{}, []string{"resolve", "observe", "read", "nak"}, 0, 1, 0, 0},
		{"ready-existing-result", "READY", 0, nil, true, authorizationDomainAttempt{}, []string{"resolve", "observe", "complete:EXECUTED", "result", "ack", "terminal"}, 1, 0, 0, 0},
		{"result-redelivery", "RESULT_CONFIRMED", 1, nil, false, authorizationDomainAttempt{}, []string{"resolve", "observe", "ack", "terminal"}, 1, 0, 0, 0},
		{"terminal-new-sequence", "TERMINAL_CONFIRMED", 1, nil, false, authorizationDomainAttempt{}, []string{"resolve", "observe", "ack"}, 1, 0, 0, 0},
		{"twelfth-grant-unknown", "DOMAIN_INFLIGHT", 12, nil, false, authorizationDomainAttempt{}, []string{"resolve", "observe", "read", "nak"}, 0, 1, 0, 0},
		{"twelfth-confirmed-failure", "DOMAIN_INFLIGHT", 12, approvalTestStringPointer("DOMAIN_PRECONDITION_REJECTED"), false, authorizationDomainAttempt{}, []string{"resolve", "observe", "read", "publish-dlq", "dlq", "ack", "terminal"}, 1, 0, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer, account, domain, delivery := authorizationConsumerFixture(t, test.state, test.attempts)
			account.read = ApprovalAuthorizationDeliveryState{DomainAttemptCount: test.attempts, State: test.state, Version: account.state.Version, LastErrorCode: test.read}
			domain.found = test.resultFound
			domain.result = authorizationDomainResult{DomainResult: json.RawMessage(`{"recovered":true}`)}
			domain.attempt = test.attempt
			consumer.Handle(context.Background(), delivery)
			if !equalStrings(account.order, test.wantOrder) || delivery.ack != test.wantAck || delivery.nak != test.wantNak || delivery.publish != test.wantPublish || domain.calls != test.wantDomain {
				t.Fatalf("unexpected outcome order=%v ack=%d nak=%d publish=%d domain=%d", account.order, delivery.ack, delivery.nak, delivery.publish, domain.calls)
			}
		})
	}
}

func TestAuthorizationCommandConsumerDefiniteAndUnknownFailures(t *testing.T) {
	for _, test := range []struct {
		name, code  string
		definite    bool
		wantOrder   []string
		wantFailure int
	}{
		{"definite", "DOMAIN_PRECONDITION_REJECTED", true, []string{"resolve", "observe", "begin", "grant", "complete:FAILED", "failure", "nak"}, 1},
		{"unknown", "", false, []string{"resolve", "observe", "begin", "grant", "nak"}, 0},
		{"sensitive-code", "ACCESS_TOKEN_REJECTED", true, []string{"resolve", "observe", "begin", "grant", "nak"}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", 0)
			domain.attempt = authorizationDomainAttempt{StableFailureCode: test.code, FailureIsDefinite: test.definite}
			consumer.Handle(context.Background(), delivery)
			if !equalStrings(account.order, test.wantOrder) || account.failure != test.wantFailure || delivery.ack != 0 || delivery.nak != 1 {
				t.Fatalf("unsafe failure handling order=%v failure=%d ack=%d nak=%d", account.order, account.failure, delivery.ack, delivery.nak)
			}
		})
	}
}

func TestProofEraReadyFailureCannotBeginOrGrantAgain(t *testing.T) {
	for _, test := range []struct {
		attempts int16
		code     *string
	}{
		{attempts: 1},
		{attempts: 1, code: approvalTestStringPointer("DOMAIN_REVISION_CHANGED")},
		{attempts: 0, code: approvalTestStringPointer("DOMAIN_REVISION_CHANGED")},
	} {
		consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", test.attempts)
		binding := account.binding
		binding.SerializationVersion = approvalencoding.ApprovalSerializationVersion
		state := account.state
		state.LastErrorCode = test.code
		if err := consumer.advance(context.Background(), delivery, binding, approvalencoding.AuthorizationEvent{}, state); err == nil || account.begin != 0 || account.grant != 0 || domain.calls != 0 {
			t.Fatalf("proof-era old READY row dispatched: attempt=%d err=%v begin=%d grant=%d domain=%d", test.attempts, err, account.begin, account.grant, domain.calls)
		}
	}
}

func TestAuthorizationCommandConsumerTwelfthDefiniteFailurePublishesThenAcknowledges(t *testing.T) {
	consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", 11)
	domain.attempt = definiteAuthorizationFailure("DOMAIN_PRECONDITION_REJECTED")
	consumer.Handle(context.Background(), delivery)
	want := []string{"resolve", "observe", "begin", "grant", "complete:FAILED", "failure", "publish-dlq", "dlq", "ack", "terminal"}
	if !equalStrings(account.order, want) || account.failure != 1 || delivery.publish != 1 || delivery.ack != 1 || delivery.nak != 0 || domain.calls != 1 {
		t.Fatalf("twelfth failure order=%v failure=%d publish=%d ack=%d nak=%d domain=%d", account.order, account.failure, delivery.publish, delivery.ack, delivery.nak, domain.calls)
	}
}

func TestAuthorizationCommandConsumerRecoversCompletedFailureWithoutAnotherDomainCall(t *testing.T) {
	consumer, account, domain, delivery := authorizationConsumerFixture(t, "DOMAIN_INFLIGHT", 3)
	status, code := "FAILED", "DOMAIN_PRECONDITION_REJECTED"
	account.read.ExecutionStatus, account.read.ExecutionErrorClass = &status, &code
	consumer.Handle(context.Background(), delivery)
	want := []string{"resolve", "observe", "read", "failure", "nak"}
	if !equalStrings(account.order, want) || account.failure != 1 || account.grant != 0 || domain.calls != 0 || delivery.ack != 0 || delivery.nak != 1 {
		t.Fatalf("failure recovery order=%v failure=%d grant=%d domain=%d ack=%d nak=%d", account.order, account.failure, account.grant, domain.calls, delivery.ack, delivery.nak)
	}
}

func TestAuthorizationCommandConsumerCrashBoundaries(t *testing.T) {
	for _, stage := range []string{"begin", "grant", "complete", "result", "ack", "terminal"} {
		t.Run(stage, func(t *testing.T) {
			consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", 0)
			domain.attempt = authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: json.RawMessage(`{"ok":true}`)}}
			account.errAt = stage
			if stage == "ack" {
				delivery.errAt = "ack"
			}
			consumer.Handle(context.Background(), delivery)
			if stage != "terminal" && stage != "ack" && delivery.ack > 0 {
				t.Fatalf("stage %s acknowledged prematurely", stage)
			}
			wantNAK := 1
			if stage == "terminal" {
				wantNAK = 0
			}
			if delivery.nak != wantNAK {
				t.Fatalf("stage %s NAK=%d want=%d", stage, delivery.nak, wantNAK)
			}
			if domain.calls > 1 {
				t.Fatalf("stage %s invoked domain twice", stage)
			}
		})
	}
}

func TestAuthorizationCommandConsumerCrashBoundariesConvergeOnRedelivery(t *testing.T) {
	tests := []struct {
		name                  string
		failureStage          string
		persistedState        string
		persistedDomainResult bool
		wantFirstAck          int
		wantFirstNAK          int
		wantSecondAck         int
		wantSecondNAK         int
		wantDomainCalls       int
	}{
		{name: "begin-not-persisted", failureStage: "begin", persistedState: "READY", wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "begin-response-lost", failureStage: "begin", persistedState: "READY", wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "grant-not-persisted", failureStage: "grant", persistedState: "READY", wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "grant-response-lost", failureStage: "grant", persistedState: "DOMAIN_INFLIGHT", wantFirstNAK: 1, wantSecondNAK: 1},
		{name: "complete-response-lost", failureStage: "complete", persistedState: "DOMAIN_INFLIGHT", persistedDomainResult: true, wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "result-not-persisted", failureStage: "result", persistedState: "DOMAIN_INFLIGHT", persistedDomainResult: true, wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "result-response-lost", failureStage: "result", persistedState: "RESULT_CONFIRMED", persistedDomainResult: true, wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "double-ack-unconfirmed", failureStage: "ack", persistedState: "RESULT_CONFIRMED", persistedDomainResult: true, wantFirstAck: 1, wantFirstNAK: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "terminal-not-persisted", failureStage: "terminal", persistedState: "RESULT_CONFIRMED", persistedDomainResult: true, wantFirstAck: 1, wantSecondAck: 1, wantDomainCalls: 1},
		{name: "terminal-response-lost", failureStage: "terminal", persistedState: "TERMINAL_CONFIRMED", persistedDomainResult: true, wantFirstAck: 1, wantSecondAck: 1, wantDomainCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			consumer, account, domain, first := authorizationConsumerFixture(t, "READY", 0)
			domain.attempt = authorizationDomainAttempt{Result: authorizationDomainResult{DomainResult: json.RawMessage(`{"ok":true}`)}}
			if test.failureStage == "ack" {
				first.errAt = "ack"
			} else {
				account.errAt = test.failureStage
			}
			consumer.Handle(context.Background(), first)
			if first.ack != test.wantFirstAck || first.nak != test.wantFirstNAK {
				t.Fatalf("first delivery ack=%d nak=%d", first.ack, first.nak)
			}

			account.errAt = ""
			account.order = nil
			account.state = ApprovalAuthorizationDeliveryState{DomainAttemptCount: 1, State: test.persistedState, Version: 10}
			account.read = account.state
			domain.found = test.persistedDomainResult && test.persistedState == "DOMAIN_INFLIGHT"
			domain.result = authorizationDomainResult{DomainResult: json.RawMessage(`{"ok":true}`)}
			second := &authorizationDeliveryStub{raw: first.raw, id: first.id, meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 42, NumDelivered: 2}, order: &account.order}
			consumer.Handle(context.Background(), second)
			if second.ack != test.wantSecondAck || second.nak != test.wantSecondNAK || domain.calls != test.wantDomainCalls {
				t.Fatalf("redelivery did not converge ack=%d nak=%d domain=%d order=%v", second.ack, second.nak, domain.calls, account.order)
			}
		})
	}
}

func TestAuthorizationCommandConsumerDLQCrashBoundariesConvergeOnRedelivery(t *testing.T) {
	for _, test := range []struct {
		name                                string
		failureStage, persistedState        string
		firstAck, firstNAK, firstPublish    int
		secondAck, secondNAK, secondPublish int
	}{
		{name: "publish-not-confirmed", failureStage: "publish", persistedState: "DOMAIN_INFLIGHT", firstNAK: 1, firstPublish: 1, secondAck: 1, secondPublish: 1},
		{name: "dlq-not-persisted", failureStage: "dlq", persistedState: "DOMAIN_INFLIGHT", firstNAK: 1, firstPublish: 1, secondAck: 1, secondPublish: 1},
		{name: "dlq-response-lost", failureStage: "dlq", persistedState: "DLQ_PUBLISHED_ACK_PENDING", firstNAK: 1, firstPublish: 1, secondAck: 1},
		{name: "double-ack-unconfirmed", failureStage: "ack", persistedState: "DLQ_PUBLISHED_ACK_PENDING", firstAck: 1, firstNAK: 1, firstPublish: 1, secondAck: 1},
		{name: "terminal-not-persisted", failureStage: "terminal", persistedState: "DLQ_PUBLISHED_ACK_PENDING", firstAck: 1, firstPublish: 1, secondAck: 1},
		{name: "terminal-response-lost", failureStage: "terminal", persistedState: "TERMINAL_CONFIRMED", firstAck: 1, firstPublish: 1, secondAck: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer, account, domain, first := authorizationConsumerFixture(t, "DOMAIN_INFLIGHT", approvalconsumer.MaxDomainAttempts)
			lastError := "DOMAIN_PRECONDITION_REJECTED"
			account.read = ApprovalAuthorizationDeliveryState{DomainAttemptCount: approvalconsumer.MaxDomainAttempts, State: "DOMAIN_INFLIGHT", Version: account.state.Version, LastErrorCode: &lastError}
			if test.failureStage == "publish" || test.failureStage == "ack" {
				first.errAt = test.failureStage
			} else {
				account.errAt = test.failureStage
			}
			consumer.Handle(context.Background(), first)
			if first.ack != test.firstAck || first.nak != test.firstNAK || first.publish != test.firstPublish || domain.calls != 0 {
				t.Fatalf("first delivery ack=%d nak=%d publish=%d domain=%d", first.ack, first.nak, first.publish, domain.calls)
			}

			account.errAt = ""
			account.order = nil
			account.state = ApprovalAuthorizationDeliveryState{DomainAttemptCount: approvalconsumer.MaxDomainAttempts, State: test.persistedState, Version: 10, LastErrorCode: &lastError}
			account.read = account.state
			second := &authorizationDeliveryStub{raw: first.raw, id: first.id, meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 42, NumDelivered: 2}, order: &account.order}
			consumer.Handle(context.Background(), second)
			if second.ack != test.secondAck || second.nak != test.secondNAK || second.publish != test.secondPublish || domain.calls != 0 {
				t.Fatalf("redelivery did not converge ack=%d nak=%d publish=%d domain=%d order=%v", second.ack, second.nak, second.publish, domain.calls, account.order)
			}
		})
	}
}

func TestAuthorizationCommandConsumerRejectsMalformedTerminalCASAfterAcknowledgement(t *testing.T) {
	consumer, account, domain, delivery := authorizationConsumerFixture(t, "RESULT_CONFIRMED", 1)
	account.terminalState = "READY"
	consumer.Handle(context.Background(), delivery)
	if domain.calls != 0 || delivery.ack != 1 || delivery.nak != 0 || account.terminal != 1 {
		t.Fatalf("malformed terminal response domain=%d ack=%d nak=%d terminal=%d", domain.calls, delivery.ack, delivery.nak, account.terminal)
	}
}

func TestAuthorizationCommandConsumerClosedDurableRegistration(t *testing.T) {
	account := &authorizationAccountStub{}
	for _, durable := range []string{
		"billing-command-executor-v1",
		"billing-wallet-adjustment-executor-p1-v1",
		"billing-payment-provider-resume-executor-p2-v1",
		"billing-payment-product-resume-executor-p1-v1",
	} {
		consumer, err := NewAuthorizationCommandConsumer(&Service{}, account, durable)
		if err != nil || consumer.durable != durable || consumer.workload != approvalconsumer.BillingWorkload {
			t.Fatalf("durable %s registration consumer=%+v err=%v", durable, consumer, err)
		}
	}
	for _, durable := range []string{"", "token-model-lifecycle-executor-v1", "billing-refund-executor-v1", " billing-command-executor-v1"} {
		if consumer, err := NewAuthorizationCommandConsumer(&Service{}, account, durable); err == nil || consumer != nil {
			t.Fatalf("non-Billing authorization durable %q was accepted", durable)
		}
	}
}

func TestAuthorizationCommandConsumerBindsDurableToSerialization(t *testing.T) {
	consumer, _, _, _ := authorizationConsumerFixture(t, "READY", 0)
	legacy := consumer
	legacyBinding := legacy.account.(*authorizationAccountStub).binding
	if !legacy.validBinding(legacyBinding) {
		t.Fatal("legacy durable rejected its legacy serialization")
	}
	legacyBinding.SerializationVersion = approvalencoding.ApprovalSerializationVersion
	if legacy.validBinding(legacyBinding) {
		t.Fatal("legacy durable accepted approval serialization")
	}

	for _, descriptor := range approvalconsumer.BillingCommands() {
		binding := legacyBinding
		binding.Durable = descriptor.Durable
		binding.CommandType = descriptor.CommandType
		binding.PolicyVersion = descriptor.PolicyVersion
		binding.SerializationVersion = approvalencoding.ApprovalSerializationVersion
		candidate := &AuthorizationCommandConsumer{account: legacy.account, domain: legacy.domain, durable: descriptor.Durable, workload: descriptor.Workload}
		if !candidate.validBinding(binding) {
			t.Fatalf("new durable %s rejected approval serialization", descriptor.Durable)
		}
		binding.SerializationVersion = approvalencoding.LegacySerializationVersion
		if candidate.validBinding(binding) {
			t.Fatalf("new durable %s accepted legacy serialization", descriptor.Durable)
		}
	}
}

func TestAuthorizationCommandConsumerRejectsCrossedSerializationBeforeDomain(t *testing.T) {
	for _, test := range []struct {
		name, durable, serialization string
	}{
		{name: "legacy-durable-approval-wire", durable: approvalconsumer.LegacyBilling().Durable, serialization: approvalencoding.ApprovalSerializationVersion},
		{name: "dedicated-durable-legacy-wire", durable: "billing-wallet-adjustment-executor-p1-v1", serialization: approvalencoding.LegacySerializationVersion},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer, account, domain, delivery := authorizationConsumerFixture(t, "READY", 0)
			consumer.durable = test.durable
			account.binding.Durable = test.durable
			account.binding.SerializationVersion = test.serialization
			consumer.Handle(context.Background(), delivery)
			if domain.calls != 0 || account.begin != 0 || account.grant != 0 || delivery.ack != 0 || delivery.nak != 1 || !equalStrings(account.order, []string{"nak"}) {
				t.Fatalf("crossed serialization reached execution order=%v domain=%d begin=%d grant=%d ack=%d nak=%d", account.order, domain.calls, account.begin, account.grant, delivery.ack, delivery.nak)
			}
		})
	}
}

func TestSameRawJSONPreservesNumberPrecision(t *testing.T) {
	if !sameRawJSON(json.RawMessage(`{"a":1,"b":{"c":1.0}}`), json.RawMessage(`{"b":{"c":1e0},"a":1.00}`)) {
		t.Fatal("equivalent JSON objects were not equal")
	}
	if sameRawJSON(json.RawMessage(`{"version":9007199254740992}`), json.RawMessage(`{"version":9007199254740993}`)) {
		t.Fatal("distinct integers above the float64 exact range were equal")
	}
	if sameRawJSON(json.RawMessage(`{"ok":true} trailing`), json.RawMessage(`{"ok":true}`)) {
		t.Fatal("trailing JSON input was accepted")
	}
}

func TestAuthorizationCommandTargetMappingIsClosed(t *testing.T) {
	for _, candidate := range []approvalencoding.AuthorizationEvent{
		{CommandType: "WALLET_ADJUSTMENT", TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: uuid.NewString()},
		{CommandType: "PAYMENT_PROVIDER_RESUME", TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: uuid.NewString()},
		{CommandType: "PAYMENT_PRODUCT_RESUME", TargetResourceType: "PAYMENT_PRODUCT_CONTROL", TargetResourceID: uuid.NewString()},
	} {
		if !validAuthorizationCommandTarget(candidate) {
			t.Fatalf("valid command target was rejected: %+v", candidate)
		}
		wrongType := candidate
		wrongType.TargetResourceType = "WRONG"
		if validAuthorizationCommandTarget(wrongType) {
			t.Fatalf("wrong target type was accepted: %+v", wrongType)
		}
		wrongID := candidate
		wrongID.TargetResourceID += " "
		if validAuthorizationCommandTarget(wrongID) {
			t.Fatalf("non-canonical target id was accepted: %+v", wrongID)
		}
	}
}

func authorizationConsumerFixture(t *testing.T, state string, attempts int16) (*AuthorizationCommandConsumer, *authorizationAccountStub, *authorizationDomainStub, *authorizationDeliveryStub) {
	t.Helper()
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	requestID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	adjustmentID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	requester := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	critical := json.RawMessage(`{"adjustment_version":1}`)
	financial := json.RawMessage(`{"delta_microcredits":5,"wallet_id":"55555555-5555-4555-8555-555555555555"}`)
	normalized := json.RawMessage(`{"adjustment_id":"` + adjustmentID.String() + `","amount_microcredits":5,"requested_by":"` + requester.String() + `","wallet_id":"55555555-5555-4555-8555-555555555555"}`)
	payload, _ := json.Marshal(map[string]any{"approval_request_id": requestID, "command_type": "WALLET_ADJUSTMENT", "command_fingerprint": repeatHex("a"), "policy_version": 1, "executor_identity": "billing-command-executor", "execution_deadline": time.Date(2026, 9, 16, 1, 0, 0, 0, time.UTC), "normalized_command": normalized, "target_resource_type": "MANUAL_ADJUSTMENT", "target_resource_id": adjustmentID.String(), "critical_resource_versions": critical, "financial_snapshot": financial})
	raw, err := (events.Event{ID: eventID, Type: approvalencoding.AuthorizationEventType, AggregateID: requestID, AggregateVersion: 7, OccurredAt: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC), TraceID: "trace", Payload: payload}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	binding := ApprovalAuthorizationBinding{EventID: eventID, Durable: "billing-command-executor-v1", Workload: "billing-command-executor", ApprovalRequestID: requestID, ApprovedRequestVersion: 7, ApprovalEpoch: 1, SerializationVersion: approvalencoding.LegacySerializationVersion, Provenance: "FROZEN_AT_APPROVAL", EventDigest: hex.EncodeToString(sum[:]), DigestKind: "DELIVERY_BYTES", CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, ExecutorIdentity: "billing-command-executor"}
	command := ApprovedCommand{ApprovalRequestID: requestID, CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, CommandFingerprint: repeatHex("a"), NormalizedCommand: normalized, TargetResourceType: "MANUAL_ADJUSTMENT", TargetResourceID: adjustmentID.String(), CriticalResourceVersions: critical, FinancialSnapshot: financial, RequesterUserID: requester, ApproverUserID: uuid.MustParse("66666666-6666-4666-8666-666666666666"), DecisionReason: "approved reason", ExecutionIdempotencyKey: "approval:key", ExecutorIdentity: "billing-command-executor", Status: "EXECUTING", AuthorizationEventID: &eventID, AuthorizationDurable: binding.Durable, AuthorizationApprovedVersion: 7, AuthorizationEventDigest: binding.EventDigest, AuthorizationDigestKind: binding.DigestKind, AuthorizationSerialization: binding.SerializationVersion, AuthorizationProvenance: binding.Provenance, AuthorizationApprovalEpoch: 1}
	account := &authorizationAccountStub{binding: binding, state: ApprovalAuthorizationDeliveryState{DomainAttemptCount: attempts, State: state, Version: 3}, command: command}
	account.read = account.state
	domain := &authorizationDomainStub{}
	consumer := &AuthorizationCommandConsumer{account: account, domain: domain, durable: binding.Durable, workload: binding.Workload}
	delivery := &authorizationDeliveryStub{raw: raw, id: eventID.String(), meta: events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 41, NumDelivered: 1}, order: &account.order}
	return consumer, account, domain, delivery
}

func repeatHex(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
func approvalTestStringPointer(value string) *string { return &value }
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
