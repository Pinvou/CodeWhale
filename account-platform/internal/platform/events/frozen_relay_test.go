package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type frozenStoreProbe struct {
	claim       FrozenClaim
	disposition FrozenDisposition
}

func (probe *frozenStoreProbe) ClaimFrozen(context.Context, string, int, time.Duration) ([]FrozenClaim, error) {
	return []FrozenClaim{probe.claim}, nil
}

func (probe *frozenStoreProbe) ProcessFrozen(ctx context.Context, _ string, _ FrozenClaim, _ int, _ time.Time, publish FrozenPublisher) (FrozenDisposition, error) {
	result := publish(ctx, FrozenMessage{EventID: probe.claim.EventID, Subject: probe.claim.EventType, Body: []byte("frozen"), MsgID: probe.claim.EventID.String()})
	probe.disposition = result.Disposition
	if result.Disposition == FrozenUnknown {
		return "", errors.New("broker outcome unknown")
	}
	return result.Disposition, nil
}

type frozenBrokerProbe struct{ err error }

func (broker frozenBrokerProbe) Publish(context.Context, string, []byte, string) error {
	return broker.err
}

func TestFrozenRelayKeepsBrokerTransportFailureUnknown(t *testing.T) {
	probe := &frozenStoreProbe{claim: FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}}
	_, err := (FrozenRelay{Store: probe, Broker: frozenBrokerProbe{err: errors.New("connection closed")}, WorkerID: "worker"}).RunOnce(context.Background())
	if err == nil || probe.disposition != FrozenUnknown {
		t.Fatalf("disposition=%q err=%v", probe.disposition, err)
	}
}

func TestFrozenRelayReportsAcknowledgedPublish(t *testing.T) {
	probe := &frozenStoreProbe{claim: FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}}
	result, err := (FrozenRelay{Store: probe, Broker: frozenBrokerProbe{}, WorkerID: "worker"}).RunOnce(context.Background())
	if err != nil || probe.disposition != FrozenPublished || result.Claimed != 1 || result.Published != 1 {
		t.Fatalf("result=%+v disposition=%q err=%v", result, probe.disposition, err)
	}
}

type frozenCallbackStore struct {
	claimFn   func(context.Context, string, int, time.Duration) ([]FrozenClaim, error)
	processFn func(context.Context, string, FrozenClaim, int, time.Time, FrozenPublisher) (FrozenDisposition, error)
}

func (store frozenCallbackStore) ClaimFrozen(ctx context.Context, worker string, limit int, lease time.Duration) ([]FrozenClaim, error) {
	return store.claimFn(ctx, worker, limit, lease)
}

func (store frozenCallbackStore) ProcessFrozen(ctx context.Context, worker string, claim FrozenClaim, attempts int, now time.Time, publish FrozenPublisher) (FrozenDisposition, error) {
	return store.processFn(ctx, worker, claim, attempts, now, publish)
}

type frozenCallbackBroker func(context.Context, string, []byte, string) error

func (broker frozenCallbackBroker) Publish(ctx context.Context, subject string, body []byte, id string) error {
	return broker(ctx, subject, body, id)
}

func TestFrozenRelayPreservesLockedBytesAndMessageIdentity(t *testing.T) {
	claim := FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 3}
	// Deliberate whitespace/order/large integer: a re-marshal changes these bytes.
	body := []byte("{ \"z\":9007199254740993,\"a\":\"原文\" }\n")
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	locked, calls := false, 0
	store := frozenCallbackStore{
		claimFn: func(_ context.Context, worker string, limit int, lease time.Duration) ([]FrozenClaim, error) {
			if worker != "frozen-worker" || limit != 100 || lease != 30*time.Second {
				t.Fatal("claim defaults or worker changed")
			}
			return []FrozenClaim{claim}, nil
		},
		processFn: func(ctx context.Context, worker string, got FrozenClaim, attempts int, timestamp time.Time, publish FrozenPublisher) (FrozenDisposition, error) {
			if got != claim || worker != "frozen-worker" || attempts != 12 || !timestamp.Equal(now) {
				t.Fatal("committed claim identity changed before processing")
			}
			locked = true
			defer func() { locked = false }()
			for _, message := range []FrozenMessage{
				{EventID: claim.EventID, Subject: claim.EventType, Body: body, MsgID: claim.EventID.String()},
				{EventID: claim.EventID, Subject: "account-platform.dead-letter." + claim.EventType, Body: body, MsgID: claim.EventID.String() + ":dead-letter"},
			} {
				if publish(ctx, message).Disposition != FrozenPublished {
					t.Fatal("successful broker acknowledgement changed")
				}
			}
			return FrozenDeadLettered, nil
		},
	}
	broker := frozenCallbackBroker(func(_ context.Context, subject string, got []byte, id string) error {
		wantSubject, wantID := claim.EventType, claim.EventID.String()
		if calls == 1 {
			wantSubject, wantID = "account-platform.dead-letter."+wantSubject, wantID+":dead-letter"
		}
		if !locked || !bytes.Equal(got, body) || subject != wantSubject || id != wantID || calls >= 2 {
			t.Fatal("frozen delivery escaped store boundary or changed bytes/identity")
		}
		calls++
		return nil
	})
	result, err := (FrozenRelay{Store: store, Broker: broker, WorkerID: "frozen-worker", Now: func() time.Time { return now }}).RunOnce(context.Background())
	if err != nil || calls != 2 || result != (RelayResult{Claimed: 1, DeadLettered: 1}) {
		t.Fatalf("wrong confirmed store outcome: %+v %v", result, err)
	}
}

func TestFrozenRelayRejectsIncompleteConfiguration(t *testing.T) {
	store := frozenCallbackStore{claimFn: func(context.Context, string, int, time.Duration) ([]FrozenClaim, error) {
		t.Fatal("invalid configuration reached claim")
		return nil, nil
	}}
	for _, relay := range []FrozenRelay{
		{Broker: frozenBrokerProbe{}, WorkerID: "worker"},
		{Store: store, WorkerID: "worker"},
		{Store: store, Broker: frozenBrokerProbe{}},
	} {
		if result, err := relay.RunOnce(context.Background()); err == nil || result != (RelayResult{}) {
			t.Fatalf("invalid configuration accepted: %+v %v", result, err)
		}
	}
}

func TestFrozenRelayDoesNotGuessUnknownBrokerOutcome(t *testing.T) {
	for _, failure := range []error{context.DeadlineExceeded, context.Canceled, io.EOF, errors.New("transport outcome not confirmed")} {
		probe := &frozenStoreProbe{claim: FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}}
		result, err := (FrozenRelay{Store: probe, Broker: frozenBrokerProbe{err: failure}, WorkerID: "worker"}).RunOnce(context.Background())
		if err == nil || probe.disposition != FrozenUnknown || result != (RelayResult{Claimed: 1}) {
			t.Fatalf("unknown broker outcome counted as a final effect: %+v %v", result, err)
		}
	}
}

func TestFrozenRelayClosedPublishErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FrozenDisposition
	}{
		{"acknowledged", nil, FrozenPublished},
		{"permissions", nats.ErrPermissionViolation, FrozenRetry},
		{"authorization", nats.ErrAuthorization, FrozenRetry},
		{"local-subject", nats.ErrBadSubject, FrozenRetry},
		{"local-size", nats.ErrMaxPayload, FrozenRetry},
		{"local-message", nats.ErrInvalidMsg, FrozenRetry},
		{"timeout", nats.ErrTimeout, FrozenUnknown},
		{"deadline", context.DeadlineExceeded, FrozenUnknown},
		{"canceled", context.Canceled, FrozenUnknown},
		{"eof", io.EOF, FrozenUnknown},
		{"closed", nats.ErrConnectionClosed, FrozenUnknown},
		{"draining", nats.ErrConnectionDraining, FrozenUnknown},
		{"reconnecting", nats.ErrConnectionReconnecting, FrozenUnknown},
		{"disconnected", nats.ErrDisconnected, FrozenUnknown},
		{"no-servers", nats.ErrNoServers, FrozenUnknown},
		{"stale", nats.ErrStaleConnection, FrozenUnknown},
		{"auth-expired", nats.ErrAuthExpired, FrozenUnknown},
		{"auth-revoked", nats.ErrAuthRevoked, FrozenUnknown},
		{"unknown-api-4xx", &jetstream.APIError{Code: 400, ErrorCode: 19999}, FrozenUnknown},
		{"unknown-api-5xx", &jetstream.APIError{Code: 503, ErrorCode: 19999}, FrozenUnknown},
		{"unknown-type", errors.New("unknown broker outcome"), FrozenUnknown},
		{"mixed-unknown-wins", errors.Join(nats.ErrBadSubject, context.DeadlineExceeded), FrozenUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyFrozenPublishError(test.err); got != test.want {
				t.Fatalf("classification=%s want=%s", got, test.want)
			}
			if test.err != nil && classifyFrozenPublishError(fmt.Errorf("wrapped: %w", test.err)) != test.want {
				t.Fatal("wrapping changed publish certainty")
			}
			probe := &frozenStoreProbe{claim: FrozenClaim{EventID: uuid.New(), EventType: "account.approval-command.authorized.v1", ClaimToken: uuid.New(), RelayControlEpoch: 1}}
			result, err := (FrozenRelay{Store: probe, Broker: frozenBrokerProbe{err: test.err}, WorkerID: "worker"}).RunOnce(context.Background())
			if probe.disposition != test.want {
				t.Fatal("actual Relay publisher did not use closed classification")
			}
			want := RelayResult{Claimed: 1}
			switch test.want {
			case FrozenPublished:
				want.Published = 1
			case FrozenRetry:
				want.Failed = 1
			}
			if result != want || (err != nil) != (test.want == FrozenUnknown) {
				t.Fatalf("Relay counted wrong certainty: %+v error=%v", result, err)
			}
		})
	}
}
