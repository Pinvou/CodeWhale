package events

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type authorizationTransportMessage struct {
	jetstream.Msg
	subject     string
	headers     nats.Header
	body        []byte
	meta        *jetstream.MsgMetadata
	metadataErr error
	ackErr      error
	nakErr      error
	ackCalls    int
	nakCalls    int
	deadline    time.Time
	delay       time.Duration
}

func (m *authorizationTransportMessage) Subject() string      { return m.subject }
func (m *authorizationTransportMessage) Headers() nats.Header { return m.headers }
func (m *authorizationTransportMessage) Data() []byte         { return m.body }
func (m *authorizationTransportMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return m.meta, m.metadataErr
}
func (m *authorizationTransportMessage) DoubleAck(ctx context.Context) error {
	m.ackCalls++
	m.deadline, _ = ctx.Deadline()
	return m.ackErr
}
func (m *authorizationTransportMessage) NakWithDelay(delay time.Duration) error {
	m.nakCalls++
	m.delay = delay
	return m.nakErr
}

func authorizationTransportFixture() *authorizationTransportMessage {
	return &authorizationTransportMessage{subject: authorizationSubject,
		headers: nats.Header{nats.MsgIdHdr: []string{uuid.NewString()}}, body: []byte(`{"untouched": 9007199254740993}`),
		meta: &jetstream.MsgMetadata{Stream: platformStream, Consumer: "billing-command-executor-v1", NumDelivered: 1,
			Sequence: jetstream.SequencePair{Stream: 7, Consumer: 1}}}
}

func TestAuthorizationRawTransportRejectsInvalidMetadata(t *testing.T) {
	cases := map[string]func(*authorizationTransportMessage){
		"wrong-subject": func(m *authorizationTransportMessage) { m.subject = "billing.refund.requested.v1" },
		"missing-id":    func(m *authorizationTransportMessage) { m.headers = nil },
		"duplicate-id": func(m *authorizationTransportMessage) {
			m.headers[nats.MsgIdHdr] = append(m.headers[nats.MsgIdHdr], m.headers.Get(nats.MsgIdHdr))
		},
		"invalid-id": func(m *authorizationTransportMessage) { m.headers.Set(nats.MsgIdHdr, "arbitrary") },
		"nil-id":     func(m *authorizationTransportMessage) { m.headers.Set(nats.MsgIdHdr, uuid.Nil.String()) },
		"metadata-error": func(m *authorizationTransportMessage) {
			m.metadataErr = errors.New("untrusted broker diagnostic")
		},
		"wrong-stream":   func(m *authorizationTransportMessage) { m.meta.Stream = "OTHER" },
		"wrong-consumer": func(m *authorizationTransportMessage) { m.meta.Consumer = "other" },
		"zero-sequence":  func(m *authorizationTransportMessage) { m.meta.Sequence.Stream = 0 },
		"zero-delivery":  func(m *authorizationTransportMessage) { m.meta.NumDelivered = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			message := authorizationTransportFixture()
			mutate(message)
			broker := &NATSBroker{clientName: "billing-command-executor"}
			if _, err := broker.newAuthorizationRawDelivery(context.Background(), message, "billing-command-executor-v1", true); !errors.Is(err, ErrAuthorizationDelivery) {
				t.Fatal("invalid transport accepted")
			}
			if message.ackCalls != 0 || message.nakCalls != 0 {
				t.Fatal("validation performed an implicit disposition")
			}
		})
	}
}

func TestAuthorizationRawTransportImmutableBytesAndBoundedAck(t *testing.T) {
	message := authorizationTransportFixture()
	original := bytes.Clone(message.body)
	broker := &NATSBroker{clientName: "billing-command-executor"}
	delivery, err := broker.newAuthorizationRawDelivery(context.Background(), message, message.meta.Consumer, true)
	if err != nil {
		t.Fatal(err)
	}
	message.body[0] = '!'
	copy := delivery.DeliveryBytes()
	copy[1] = '!'
	if !bytes.Equal(delivery.DeliveryBytes(), original) {
		t.Fatal("caller mutated immutable delivery bytes")
	}
	wantErr := errors.New("ack is unknown")
	message.ackErr = wantErr
	started := time.Now()
	if !errors.Is(delivery.DoubleAck(context.Background()), wantErr) || message.ackCalls != 1 || message.deadline.IsZero() || message.deadline.After(started.Add(authorizationOperationTimeout+time.Second)) {
		t.Fatal("ACK error or bounded context lost")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(delivery.DoubleAck(cancelled), context.Canceled) || message.ackCalls != 1 {
		t.Fatal("cancelled ACK dispatched")
	}
	message.nakErr = wantErr
	if !errors.Is(delivery.NakWithDelay(512*time.Second), wantErr) || message.delay != 512*time.Second {
		t.Fatal("NAK result lost")
	}
	for _, delay := range []time.Duration{-time.Second, 0, 513 * time.Second} {
		if !errors.Is(delivery.NakWithDelay(delay), ErrAuthorizationDelivery) {
			t.Fatal("invalid NAK delay accepted")
		}
	}
	if message.nakCalls != 1 {
		t.Fatal("invalid NAK reached transport")
	}
}

func TestAuthorizationRawTransportDLQFixedBytesAndReceipt(t *testing.T) {
	message := authorizationTransportFixture()
	broker := &NATSBroker{clientName: "billing-command-executor"}
	delivery, err := broker.newAuthorizationRawDelivery(context.Background(), message, message.meta.Consumer, true)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	delivery.publish = func(ctx context.Context, body []byte, id string) (*jetstream.PubAck, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok || !bytes.Equal(body, message.body) || id != delivery.MessageID()+":"+message.meta.Consumer+":dead-letter" {
			t.Fatal("unbounded or altered DLQ publish")
		}
		body[0] = '!'
		return &jetstream.PubAck{Stream: platformStream, Sequence: 33, Duplicate: true}, nil
	}
	for range 2 {
		ack, err := delivery.PublishDeadLetter(context.Background())
		if err != nil || ack.Stream != platformStream || ack.StreamSequence != 33 || !ack.Duplicate {
			t.Fatal("DLQ receipt lost")
		}
	}
	if calls != 2 || message.ackCalls != 0 {
		t.Fatal("publish implicitly acknowledged original delivery")
	}
	for name, ack := range map[string]*jetstream.PubAck{"nil": nil, "wrong-stream": {Stream: "OTHER", Sequence: 1}, "zero-sequence": {Stream: platformStream}} {
		t.Run(name, func(t *testing.T) {
			delivery.publish = func(context.Context, []byte, string) (*jetstream.PubAck, error) { return ack, nil }
			if _, err := delivery.PublishDeadLetter(context.Background()); !errors.Is(err, ErrAuthorizationDelivery) {
				t.Fatal("invalid PubAck accepted")
			}
		})
	}
	broker.clientName = "token-command-executor"
	token, err := broker.newAuthorizationRawDelivery(context.Background(), message, message.meta.Consumer, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := token.PublishDeadLetter(context.Background()); !errors.Is(err, ErrAuthorizationDelivery) {
		t.Fatal("Token acquired Billing DLQ operation")
	}
}

func TestAuthorizationRawTransportCallbackLifetimeAndConcurrentDisposition(t *testing.T) {
	message := authorizationTransportFixture()
	broker := &NATSBroker{}
	delivery, err := broker.newAuthorizationRawDelivery(context.Background(), message, message.meta.Consumer, true)
	if err != nil {
		t.Fatal(err)
	}
	// Hold a real operation across the attempt to start a concurrent one.
	_, finish, err := delivery.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(delivery.DoubleAck(context.Background()), ErrAuthorizationDelivery) || message.ackCalls != 0 {
		t.Fatal("concurrent disposition was queued or dispatched")
	}
	finish()
	delivery.deactivate()
	if !errors.Is(delivery.DoubleAck(context.Background()), ErrAuthorizationDelivery) || !errors.Is(delivery.NakWithDelay(time.Second), ErrAuthorizationDelivery) {
		t.Fatal("closed callback retained ACK/NAK capability")
	}
	if _, err := delivery.PublishDeadLetter(context.Background()); !errors.Is(err, ErrAuthorizationDelivery) {
		t.Fatal("closed callback retained DLQ capability")
	}
	if message.ackCalls != 0 || message.nakCalls != 0 {
		t.Fatal("late operation reached the broker")
	}
	parent, cancel := context.WithCancel(context.Background())
	live, err := broker.newAuthorizationRawDelivery(parent, message, message.meta.Consumer, false)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(live.DoubleAck(context.Background()), ErrAuthorizationDelivery) || message.ackCalls != 0 {
		t.Fatal("cancelled generation retained delivery capability")
	}
}
