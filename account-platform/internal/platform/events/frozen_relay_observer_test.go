package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

type frozenObserverProbe struct {
	active int
	begins int
	ends   int
}

func (probe *frozenObserverProbe) BeginFrozenPublish() {
	probe.active++
	probe.begins++
}

func (probe *frozenObserverProbe) EndFrozenPublish() {
	probe.active--
	probe.ends++
}

func TestFrozenRelayObservesEveryActualMainAndDeadLetterPublish(t *testing.T) {
	claim := FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}
	observer := &frozenObserverProbe{}
	calls := 0
	store := frozenCallbackStore{
		claimFn: func(context.Context, string, int, time.Duration) ([]FrozenClaim, error) {
			return []FrozenClaim{claim}, nil
		},
		processFn: func(ctx context.Context, _ string, _ FrozenClaim, _ int, _ time.Time, publish FrozenPublisher) (FrozenDisposition, error) {
			for _, subject := range []string{claim.EventType, "account-platform.dead-letter." + claim.EventType} {
				if result := publish(ctx, FrozenMessage{EventID: claim.EventID, Subject: subject, Body: []byte("frozen"), MsgID: claim.EventID.String()}); result.Disposition != FrozenPublished {
					t.Fatalf("publish disposition = %s", result.Disposition)
				}
			}
			return FrozenDeadLettered, nil
		},
	}
	broker := frozenCallbackBroker(func(context.Context, string, []byte, string) error {
		if observer.active != 1 {
			t.Fatalf("active handlers inside Broker.Publish = %d", observer.active)
		}
		calls++
		return nil
	})
	result, err := (FrozenRelay{Store: store, Broker: broker, WorkerID: "worker", Observer: observer}).RunOnce(context.Background())
	if err != nil || result.DeadLettered != 1 || calls != 2 || observer.active != 0 || observer.begins != 2 || observer.ends != 2 {
		t.Fatalf("result=%+v calls=%d observer=%+v err=%v", result, calls, observer, err)
	}
}

func TestFrozenRelayEndsObserverForEveryBrokerReturnPath(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "published"},
		{name: "retry", err: nats.ErrBadSubject},
		{name: "unknown", err: errors.New("transport outcome unknown")},
		{name: "canceled", err: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			observer := &frozenObserverProbe{}
			probe := &frozenStoreProbe{claim: FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}}
			_, _ = (FrozenRelay{Store: probe, Broker: frozenCallbackBroker(func(context.Context, string, []byte, string) error {
				if observer.active != 1 {
					t.Fatalf("active handlers inside Broker.Publish = %d", observer.active)
				}
				return test.err
			}), WorkerID: "worker", Observer: observer}).RunOnce(context.Background())
			if observer.active != 0 || observer.begins != 1 || observer.ends != 1 {
				t.Fatalf("observer not balanced: %+v", observer)
			}
		})
	}
}

func TestFrozenRelayDoesNotObserveWhenBrokerIsNotCalled(t *testing.T) {
	observer := &frozenObserverProbe{}
	store := frozenCallbackStore{
		claimFn: func(context.Context, string, int, time.Duration) ([]FrozenClaim, error) { return nil, nil },
		processFn: func(context.Context, string, FrozenClaim, int, time.Time, FrozenPublisher) (FrozenDisposition, error) {
			t.Fatal("empty claim batch reached processing")
			return "", nil
		},
	}
	result, err := (FrozenRelay{Store: store, Broker: frozenBrokerProbe{}, WorkerID: "worker", Observer: observer}).RunOnce(context.Background())
	if err != nil || result != (RelayResult{}) || *observer != (frozenObserverProbe{}) {
		t.Fatalf("result=%+v observer=%+v err=%v", result, observer, err)
	}
}
