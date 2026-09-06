package token

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
)

type failingApprovalRegistrationBroker struct {
	contexts                  []context.Context
	stops                     int
	stoppedBeforeCancellation bool
}

func (b *failingApprovalRegistrationBroker) ConsumeUntilResolved(ctx context.Context, _, _ string, _ events.ConsumerHandler) (func(), error) {
	b.contexts = append(b.contexts, ctx)
	if len(b.contexts) == 2 {
		return nil, errors.New("fixture second registration failure")
	}
	return func() {
		b.stops++
		b.stoppedBeforeCancellation = ctx.Err() == nil
	}, nil
}

func TestApprovalConsumerRegistrationFailureClosesEpoch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	broker := &failingApprovalRegistrationBroker{}
	reader := &approvalActivationStub{state: "ACTIVE", epoch: 1}
	err := RunTokenApprovalConsumers(ctx, broker, reader, "http://token.integration.invalid", "http://account.integration.invalid", "fixture-service-token")
	if err == nil || len(broker.contexts) != 2 || broker.stops != 1 || broker.stoppedBeforeCancellation {
		t.Fatalf("partial registration was not retired safely: err=%v registrations=%d stops=%d wrong_order=%v", err, len(broker.contexts), broker.stops, broker.stoppedBeforeCancellation)
	}
	if ctx.Err() != nil {
		t.Fatal("registration failure cancelled the caller or only returned on timeout")
	}
	for _, registered := range broker.contexts {
		if registered.Err() == nil {
			t.Fatal("failed registration retained a live generation context")
		}
	}
}
