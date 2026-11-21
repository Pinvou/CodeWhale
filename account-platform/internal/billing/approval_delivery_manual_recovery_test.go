package billing

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

type manualRecoveryAckAccount struct {
	approvalAuthorizationAccount
	delivery  *tokenRouteDelivery
	terminals int
}

func (a *manualRecoveryAckAccount) RecordAuthorizationTerminal(_ context.Context, _ ApprovalAuthorizationBinding, version int64) (ApprovalAuthorizationDeliveryState, error) {
	if a.delivery.acks < 1 {
		return ApprovalAuthorizationDeliveryState{}, errors.New("terminal before broker confirmation")
	}
	a.terminals++
	return ApprovalAuthorizationDeliveryState{DomainAttemptCount: 1, State: "TERMINAL_CONFIRMED", Version: version + 1}, nil
}

type manualRecoveryForbiddenDomain struct{ approvalAuthorizationDomain }

func TestManualGrantRecoveryConsumerAcknowledgesWithoutExecution(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	binding := ApprovalAuthorizationBinding{EventID: uuid.New(), ApprovalRequestID: uuid.New(), Durable: descriptor.Durable, Workload: descriptor.Workload}
	delivery := &tokenRouteDelivery{ackError: errors.New("controlled unconfirmed ACK")}
	account := &manualRecoveryAckAccount{delivery: delivery}
	consumer := &AuthorizationCommandConsumer{account: account, domain: &manualRecoveryForbiddenDomain{}, durable: descriptor.Durable, workload: descriptor.Workload}
	// This is the existing Account wire shape produced by a manually confirmed
	// unstarted grant: no execution status and no synthetic execution reference.
	state := ApprovalAuthorizationDeliveryState{State: "RESULT_CONFIRMED", DomainAttemptCount: 1, Version: 3}
	if !validAuthorizationDeliveryState(state) {
		t.Fatal("manual result is incompatible with existing consumer")
	}
	if err := consumer.advance(context.Background(), delivery, binding, approvalencoding.AuthorizationEvent{}, state); err == nil || account.terminals != 0 {
		t.Fatal("unconfirmed broker ACK became terminal")
	}
	delivery.ackError = nil
	if err := consumer.advance(context.Background(), delivery, binding, approvalencoding.AuthorizationEvent{}, state); err != nil || account.terminals != 1 || delivery.acks != 2 || delivery.naks != 0 {
		t.Fatal("confirmed manual result did not converge", err)
	}
	// A duplicate terminal delivery only confirms ACK again. All embedded
	// account/domain methods panic if Begin/Grant/Lookup/Execute is attempted.
	state.State = "TERMINAL_CONFIRMED"
	state.Version++
	if err := consumer.advance(context.Background(), delivery, binding, approvalencoding.AuthorizationEvent{}, state); err != nil || account.terminals != 1 || delivery.acks != 3 {
		t.Fatal("terminal duplicate repeated business work", err)
	}
}
